package activity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// Read-only queries the activities need and internal/store does not yet
// offer: a plan's body (CreatePlan returns the row without it, leaving
// its readers to decode it), and rows looked up by ID rather than by
// natural key. Every write still goes through the store's functions.
// These are candidates to move into internal/store; ticket 8 was allowed
// one change outside its own files (ListIssueComments), so they live
// here, read-only.

// planContext is what an attempt needs from the rows above it.
type planContext struct {
	TaskStatus   contracts.TaskStatus
	TaskRevision int
	TaskSpec     contracts.TaskSpec

	PlanID       string
	PlanStatus   contracts.PlanStatus
	PlanRevision int
	BaseSHA      string
	Body         []byte

	FeatureRequestID string
	RepoID           string
	RepoFullName     string
}

func readPlanContext(ctx context.Context, db store.DBTX, taskID string) (planContext, error) {
	const query = `SELECT t.state, t.revision, t.spec, p.id::text, p.state, p.revision, coalesce(p.base_sha, ''),
			p.body, f.id::text, r.id::text, r.full_name
		FROM tasks t
		JOIN plans p ON p.id = t.plan_id
		JOIN feature_requests f ON f.id = p.feature_request_id
		JOIN repos r ON r.id = f.repo_id
		WHERE t.id = $1`
	if err := checkID("task", taskID); err != nil {
		return planContext{}, err
	}
	var pc planContext
	var spec []byte
	err := db.QueryRow(ctx, query, taskID).Scan(&pc.TaskStatus, &pc.TaskRevision, &spec, &pc.PlanID, &pc.PlanStatus,
		&pc.PlanRevision, &pc.BaseSHA, &pc.Body, &pc.FeatureRequestID, &pc.RepoID, &pc.RepoFullName)
	if err != nil {
		return planContext{}, notFound(err, "task", taskID)
	}
	if err := json.Unmarshal(spec, &pc.TaskSpec); err != nil {
		return planContext{}, fmt.Errorf("decoding spec of task %s: %w", taskID, err)
	}
	return pc, nil
}

// planRow is a plan looked up by ID.
type planRow struct {
	FeatureRequestID string
	Status           contracts.PlanStatus
	Revision         int
	BaseSHA          string
	Body             []byte
}

func readPlan(ctx context.Context, db store.DBTX, planID string) (planRow, error) {
	const query = `SELECT feature_request_id::text, state, revision, coalesce(base_sha, ''), body FROM plans WHERE id = $1`
	if err := checkID("plan", planID); err != nil {
		return planRow{}, err
	}
	var p planRow
	err := db.QueryRow(ctx, query, planID).Scan(&p.FeatureRequestID, &p.Status, &p.Revision, &p.BaseSHA, &p.Body)
	if err != nil {
		return planRow{}, notFound(err, "plan", planID)
	}
	return p, nil
}

func readTask(ctx context.Context, db store.DBTX, taskID string) (TaskRef, error) {
	const query = `SELECT id::text, key, state, revision FROM tasks WHERE id = $1`
	if err := checkID("task", taskID); err != nil {
		return TaskRef{}, err
	}
	var t TaskRef
	if err := db.QueryRow(ctx, query, taskID).Scan(&t.TaskID, &t.Key, &t.Status, &t.Revision); err != nil {
		return TaskRef{}, notFound(err, "task", taskID)
	}
	return t, nil
}

// attemptRow is an attempt looked up by ID, with its cleanup columns.
type attemptRow struct {
	AttemptID    string
	TaskID       string
	Ordinal      int
	State        contracts.AttemptState
	Reason       contracts.FailureReason
	Revision     int
	ArtifactHash string
	SandboxID    string
	Released     bool
}

const attemptRowColumns = `id::text, task_id::text, ordinal, state, coalesce(reason, ''), revision,
	coalesce(artifact_hash, ''), coalesce(sandbox_id, ''), sandbox_released_at IS NOT NULL`

func scanAttemptRow(row pgx.Row) (attemptRow, error) {
	var r attemptRow
	err := row.Scan(&r.AttemptID, &r.TaskID, &r.Ordinal, &r.State, &r.Reason, &r.Revision,
		&r.ArtifactHash, &r.SandboxID, &r.Released)
	return r, err
}

func readAttempt(ctx context.Context, db store.DBTX, attemptID string) (attemptRow, error) {
	if err := checkID("attempt", attemptID); err != nil {
		return attemptRow{}, err
	}
	r, err := scanAttemptRow(db.QueryRow(ctx, `SELECT `+attemptRowColumns+` FROM attempts WHERE id = $1`, attemptID))
	if err != nil {
		return attemptRow{}, notFound(err, "attempt", attemptID)
	}
	return r, nil
}

// outcome is the attempt's recorded outcome.
func (r attemptRow) outcome() AttemptOutcome {
	return AttemptOutcome{AttemptID: r.AttemptID, State: r.State, Reason: r.Reason, ArtifactHash: r.ArtifactHash}
}

// attemptsWhere lists attempts matching a fixed condition. where is a
// constant from this file, never input.
func attemptsWhere(ctx context.Context, db store.Querier, where string, arg string) ([]attemptRow, error) {
	rows, err := db.Query(ctx, `SELECT `+attemptRowColumns+` FROM attempts WHERE `+where+` ORDER BY ordinal, id`, arg)
	if err != nil {
		return nil, fmt.Errorf("listing attempts: %w", err)
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		r, err := scanAttemptRow(rows)
		if err != nil {
			return nil, fmt.Errorf("listing attempts: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing attempts: %w", err)
	}
	return out, nil
}

const (
	// whereTask is every attempt of one task.
	whereTask = `task_id = $1`
	// whereFeatureUnreleased is every attempt of one feature request
	// whose recorded sandbox is not yet released.
	whereFeatureUnreleased = `sandbox_id IS NOT NULL AND sandbox_released_at IS NULL AND task_id IN (
		SELECT t.id FROM tasks t JOIN plans p ON p.id = t.plan_id WHERE p.feature_request_id = $1)`
)

// checkID refuses a malformed ID before it reaches a query, as the store
// does, so a bad input is a permanent refusal rather than a retried
// database error.
func checkID(kind, id string) error {
	if !isUUID(id) {
		return fmt.Errorf("%w: %s ID %q: %w", errNonRetryable, kind, id, store.ErrInvalidID)
	}
	return nil
}

func notFound(err error, kind, id string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s %s: %w", kind, id, store.ErrNotFound)
	}
	return fmt.Errorf("reading %s %s: %w", kind, id, err)
}

// isUUID accepts the canonical 8-4-4-4-12 hex form Postgres writes.
func isUUID(v string) bool {
	if len(v) != 36 {
		return false
	}
	for i := range len(v) {
		c := v[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !isHex(c) {
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
