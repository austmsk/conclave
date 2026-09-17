package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
)

// Sentinel errors returned by Transition. A caller retrying after a crash
// checks ErrAlreadyApplied and carries on; a caller that hits a
// WrongStateError has lost a race (a cancellation arrived first, say) and
// must reconsider.
var (
	// ErrAlreadyApplied means the record is already in the requested state.
	// On a retried activity or a duplicate webhook this is success, not
	// failure: the earlier attempt landed but its result was not observed.
	ErrAlreadyApplied = errors.New("transition already applied")

	// ErrNotFound means no record with the given id exists.
	ErrNotFound = errors.New("record not found")
)

// WrongStateError means the record exists but is in neither the expected
// state nor the requested one. Somebody else moved it first.
type WrongStateError struct {
	Kind     contracts.EntityKind
	ID       string
	Expected string
	Actual   string
}

func (e *WrongStateError) Error() string {
	return fmt.Sprintf("%s %s is %q, expected %q", e.Kind, e.ID, e.Actual, e.Expected)
}

// IllegalTransitionError means the transition table forbids the move. It is
// returned before any SQL runs.
type IllegalTransitionError struct {
	Kind contracts.EntityKind
	From string
	To   string
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("%s may not move from %q to %q", e.Kind, e.From, e.To)
}

// TransitionRequest describes one state change.
type TransitionRequest struct {
	Kind contracts.EntityKind
	ID   string

	// From is the state the caller believes the record is in. The update is
	// conditional on it.
	From string
	To   string

	// Actor is who caused the change, for the audit log (FR-SEC10). Required.
	Actor string

	// Reason is stored on the record and in the audit row. Optional.
	Reason string
}

// tables maps each entity kind to the table holding its state column. It is
// a fixed map, never derived from input, so the table name interpolated into
// SQL below cannot be influenced by a caller.
var tables = map[contracts.EntityKind]string{
	contracts.EntityFeatureRequest: "feature_requests",
	contracts.EntityPlan:           "plans",
	contracts.EntityTask:           "tasks",
	contracts.EntityAttempt:        "attempts",
}

// Transition moves a record from one state to another with compare-and-set,
// and records the move in state_transitions. Both writes happen in tx, so
// the audit row exists if and only if the state change was committed.
//
// A zero-row update is classified by re-reading the record: ErrNotFound,
// ErrAlreadyApplied, or a WrongStateError. Telling the last two apart is the
// point of the pattern (RL-13): a retry after a successful-but-unreported
// update must not be treated as a conflict.
func Transition(ctx context.Context, tx pgx.Tx, req TransitionRequest) error {
	if req.Actor == "" {
		return errors.New("transition requires an actor")
	}
	if !contracts.CanTransition(req.Kind, req.From, req.To) {
		return &IllegalTransitionError{Kind: req.Kind, From: req.From, To: req.To}
	}
	table := tables[req.Kind] // present: CanTransition rejected unknown kinds

	update := fmt.Sprintf(
		`UPDATE %s SET state = $1, reason = NULLIF($2, ''), updated_at = now()
		 WHERE id = $3 AND state = $4`, table)
	tag, err := tx.Exec(ctx, update, req.To, req.Reason, req.ID, req.From)
	if err != nil {
		return fmt.Errorf("updating %s %s: %w", req.Kind, req.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return classifyMiss(ctx, tx, table, req)
	}

	const audit = `INSERT INTO state_transitions
		(entity_kind, entity_id, from_state, to_state, actor, reason)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`
	_, err = tx.Exec(ctx, audit, req.Kind, req.ID, req.From, req.To, req.Actor, req.Reason)
	if err != nil {
		return fmt.Errorf("recording transition of %s %s: %w", req.Kind, req.ID, err)
	}
	return nil
}

// classifyMiss explains why a compare-and-set matched no rows.
func classifyMiss(ctx context.Context, tx pgx.Tx, table string, req TransitionRequest) error {
	var actual string
	query := fmt.Sprintf(`SELECT state FROM %s WHERE id = $1`, table)
	err := tx.QueryRow(ctx, query, req.ID).Scan(&actual)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s %s: %w", req.Kind, req.ID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading state of %s %s: %w", req.Kind, req.ID, err)
	}
	if actual == req.To {
		return fmt.Errorf("%s %s to %q: %w", req.Kind, req.ID, req.To, ErrAlreadyApplied)
	}
	return &WrongStateError{Kind: req.Kind, ID: req.ID, Expected: req.From, Actual: actual}
}
