package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/austmsk/conclave/internal/contracts"
)

// SQLStateUnattributedStateChange is raised by the record_state_transition
// trigger when a state column changes without an actor set on the
// transaction. It is what a direct UPDATE that bypasses Transition sees.
const SQLStateUnattributedStateChange = "CO001"

// Sentinel errors returned by Transition and ApprovePlan. A caller retrying
// after a crash checks ErrAlreadyApplied and carries on; a caller that hits
// a WrongStateError has lost a race (a cancellation arrived first, say) and
// must reconsider.
var (
	// ErrAlreadyApplied means this exact transition, from this revision,
	// has already landed. On a retried activity or a duplicate webhook this
	// is success, not failure: the earlier attempt committed but its result
	// was not observed.
	ErrAlreadyApplied = errors.New("transition already applied")

	// ErrNotFound means no record with the given id exists.
	ErrNotFound = errors.New("record not found")

	// ErrApprovalNeedsBaseSHA means a plan was sent to Transition with
	// PlanApproved as the target. Approval pins a base commit (FR-S19) and
	// the schema refuses an approved plan without one, so it has its own
	// entry point, ApprovePlan.
	ErrApprovalNeedsBaseSHA = errors.New("approving a plan requires a base SHA: use ApprovePlan")

	// ErrAnotherPlanApproved means a different plan version of the same
	// feature request was approved first. The partial unique index refuses
	// a second one; the caller should reload and decide, not retry.
	ErrAnotherPlanApproved = errors.New("another plan for this feature request is already approved")
)

// WrongStateError means the record exists but is not at the expected state
// and revision, and the requested transition is not the one that moved it.
// Somebody else got there first.
type WrongStateError struct {
	Kind             contracts.EntityKind
	ID               string
	Expected         string
	Actual           string
	ExpectedRevision int
	ActualRevision   int
}

func (e *WrongStateError) Error() string {
	return fmt.Sprintf("%s %s is %q at revision %d, expected %q at revision %d",
		e.Kind, e.ID, e.Actual, e.ActualRevision, e.Expected, e.ExpectedRevision)
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

	// From and Revision are the state and revision the caller last read.
	// The update is conditional on both. Matching on state alone is not
	// enough: tasks and feature requests have cyclic lifecycles, and a
	// retried activity that matched on state alone could re-apply itself
	// after the owner had already moved the record on and back.
	From     string
	Revision int
	To       string

	// Actor is who caused the change, for the audit log (FR-SEC10).
	// Required. It is self-reported: the audit row also records the
	// authenticated database login separately.
	Actor string

	// Reason is stored on the record and in the audit row. It replaces
	// whatever reason the record had: a reason explains the state it was
	// recorded with, so it does not survive leaving that state.
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

// Transition moves a record from one state to another with compare-and-set
// on state and revision. The record_state_transition trigger bumps the
// revision and writes the audit row in the same statement, so the row
// exists if and only if the change is committed.
//
// A zero-row update is classified by re-reading the record: ErrNotFound,
// ErrAlreadyApplied (the record is at the requested state and exactly one
// revision past the expected one, so this transition is the one that moved
// it), or a WrongStateError. Telling the last two apart is the point of the
// pattern (RL-13): a retry after a successful-but-unreported update must
// not be treated as a conflict, and a stale retry must not be re-applied.
//
// tx must run at READ COMMITTED, Postgres's default. Under that level an
// UPDATE that blocks on a row another transaction is changing re-evaluates
// its WHERE clause after the other commit, and the re-read sees the
// committed state, which is what makes the classification correct under
// real concurrency. At REPEATABLE READ or SERIALIZABLE the same collision
// surfaces as a serialization failure (SQLSTATE 40001) instead.
func Transition(ctx context.Context, tx pgx.Tx, req TransitionRequest) error {
	if err := validate(req); err != nil {
		return err
	}
	if req.Kind == contracts.EntityPlan && req.To == string(contracts.PlanApproved) {
		return ErrApprovalNeedsBaseSHA
	}
	return transition(ctx, tx, req)
}

// ApprovePlan moves a proposed plan at the given revision to approved and
// pins the base commit every task will run against (FR-S19). Retry and race
// semantics are those of Transition, plus ErrAnotherPlanApproved when a
// different plan version won.
func ApprovePlan(ctx context.Context, tx pgx.Tx, planID string, revision int, baseSHA, actor string) error {
	if baseSHA == "" {
		return ErrApprovalNeedsBaseSHA
	}
	req := TransitionRequest{
		Kind: contracts.EntityPlan, ID: planID, Revision: revision,
		From: string(contracts.PlanProposed), To: string(contracts.PlanApproved),
		Actor: actor,
	}
	if err := validate(req); err != nil {
		return err
	}
	// Conditional on state and revision so a retry against an
	// already-approved plan cannot overwrite the SHA that was pinned the
	// first time.
	const pin = `UPDATE plans SET base_sha = $1, approved_at = now()
		WHERE id = $2 AND state = $3 AND revision = $4`
	if _, err := tx.Exec(ctx, pin, baseSHA, planID, req.From, revision); err != nil {
		return fmt.Errorf("pinning base sha on plan %s: %w", planID, err)
	}
	err := transition(ctx, tx, req)
	if isConstraint(err, "plans_one_approved_per_feature_request") {
		return fmt.Errorf("approving plan %s: %w", planID, ErrAnotherPlanApproved)
	}
	return err
}

func isConstraint(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == name
}

func validate(req TransitionRequest) error {
	if req.Actor == "" {
		return errors.New("transition requires an actor")
	}
	if req.Revision < 0 {
		return fmt.Errorf("transition of %s %s: negative revision %d", req.Kind, req.ID, req.Revision)
	}
	if !contracts.CanTransition(req.Kind, req.From, req.To) {
		return &IllegalTransitionError{Kind: req.Kind, From: req.From, To: req.To}
	}
	return nil
}

func transition(ctx context.Context, tx pgx.Tx, req TransitionRequest) error {
	table := tables[req.Kind] // present: validate rejected unknown kinds

	// The trigger reads the actor from this transaction-local setting and
	// refuses the update without it. It is cleared again below so nothing
	// else in the transaction inherits the attribution.
	if _, err := tx.Exec(ctx, `SELECT set_config('conclave.actor', $1, true)`, req.Actor); err != nil {
		return fmt.Errorf("setting actor for %s %s: %w", req.Kind, req.ID, err)
	}
	update := fmt.Sprintf(
		`UPDATE %s SET state = $1, reason = NULLIF($2, ''), updated_at = now()
		 WHERE id = $3 AND state = $4 AND revision = $5`, table)
	tag, err := tx.Exec(ctx, update, req.To, req.Reason, req.ID, req.From, req.Revision)
	if err != nil {
		return fmt.Errorf("updating %s %s: %w", req.Kind, req.ID, err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('conclave.actor', '', true)`); err != nil {
		return fmt.Errorf("clearing actor for %s %s: %w", req.Kind, req.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return classifyMiss(ctx, tx, table, req)
	}
	return nil
}

// classifyMiss explains why a compare-and-set matched no rows.
func classifyMiss(ctx context.Context, tx pgx.Tx, table string, req TransitionRequest) error {
	var actual string
	var revision int
	query := fmt.Sprintf(`SELECT state, revision FROM %s WHERE id = $1`, table)
	err := tx.QueryRow(ctx, query, req.ID).Scan(&actual, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s %s: %w", req.Kind, req.ID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading state of %s %s: %w", req.Kind, req.ID, err)
	}
	// Exactly one revision on from where we expected, and in the state we
	// wanted: the only transition that could have produced that is ours.
	if actual == req.To && revision == req.Revision+1 {
		return fmt.Errorf("%s %s to %q: %w", req.Kind, req.ID, req.To, ErrAlreadyApplied)
	}
	return &WrongStateError{
		Kind: req.Kind, ID: req.ID,
		Expected: req.From, Actual: actual,
		ExpectedRevision: req.Revision, ActualRevision: revision,
	}
}
