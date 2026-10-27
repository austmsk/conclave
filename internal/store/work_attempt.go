package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
)

// Querier is what a read of several rows needs from a connection: a
// *pgxpool.Pool, a *pgx.Conn, or a pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// AttemptParams describes an attempt to create.
type AttemptParams struct {
	TaskID  string
	Ordinal int

	// Provenance is what the router is configured to use for the role.
	// Every field is required: the columns are NOT NULL because Milestone
	// 2 groups results by them. FinishAttempt records the model that
	// actually answered.
	Provenance contracts.Provenance
}

const attemptColumns = `id::text, task_id::text, ordinal, state, coalesce(reason, ''), revision,
	role, prompt_version, model, provider,
	input_tokens, output_tokens, cost_usd::float8, steps,
	coalesce(artifact_hash, ''), report, started_at, finished_at, sandbox_released_at`

// scanAttempt reads attemptColumns, plus any extra destinations after
// them.
func scanAttempt(row pgx.Row, extra ...any) (contracts.Attempt, error) {
	var a contracts.Attempt
	var report []byte
	var finished, released *time.Time
	dest := []any{&a.ID, &a.TaskID, &a.Ordinal, &a.State, &a.Reason, &a.Revision,
		&a.Provenance.Role, &a.Provenance.PromptVersion, &a.Provenance.Model, &a.Provenance.Provider,
		&a.Usage.InputTokens, &a.Usage.OutputTokens, &a.Usage.CostUSD, &a.Usage.Steps,
		&a.ArtifactHash, &report, &a.StartedAt, &finished, &released}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return contracts.Attempt{}, err
	}
	a.StartedAt, a.FinishedAt, a.SandboxReleasedAt = a.StartedAt.UTC(), utc(finished), utc(released)
	if report != nil {
		a.Report = new(contracts.CompletionReport)
		if err := json.Unmarshal(report, a.Report); err != nil {
			return contracts.Attempt{}, fmt.Errorf("decoding report of attempt %s: %w", a.ID, err)
		}
	}
	return a, nil
}

// CreateAttempt records an attempt at a task in state running and reports
// whether this call created it. The natural key is (task, ordinal): a
// second call returns the existing row unchanged, whatever provenance it
// was given. An unknown task is ErrNotFound.
func CreateAttempt(ctx context.Context, db DBTX, p AttemptParams) (contracts.Attempt, bool, error) {
	if err := checkAttemptParams(p); err != nil {
		return contracts.Attempt{}, false, fmt.Errorf("creating attempt: %w", err)
	}
	pv := p.Provenance
	const insert = `INSERT INTO attempts (task_id, ordinal, state, role, prompt_version, model, provider)
		SELECT $1::uuid, $2::integer, 'running', $3::text, $4::text, $5::text, $6::text
		WHERE EXISTS (SELECT 1 FROM tasks WHERE id = $1)
		ON CONFLICT (task_id, ordinal) DO NOTHING
		RETURNING ` + attemptColumns
	a, err := scanAttempt(db.QueryRow(ctx, insert, p.TaskID, p.Ordinal, string(pv.Role), pv.PromptVersion, pv.Model, pv.Provider))
	if err == nil {
		return a, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return contracts.Attempt{}, false, fmt.Errorf("creating attempt %d of task %s: %w", p.Ordinal, p.TaskID, err)
	}
	const existing = `SELECT ` + attemptColumns + ` FROM attempts WHERE task_id = $1 AND ordinal = $2`
	a, err = scanAttempt(db.QueryRow(ctx, existing, p.TaskID, p.Ordinal))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Attempt{}, false, fmt.Errorf("creating attempt %d: task %s: %w", p.Ordinal, p.TaskID, ErrNotFound)
	}
	if err != nil {
		return contracts.Attempt{}, false, fmt.Errorf("reading attempt %d of task %s: %w", p.Ordinal, p.TaskID, err)
	}
	return a, false, nil
}

func checkAttemptParams(p AttemptParams) error {
	if err := checkUUID("task ID", p.TaskID); err != nil {
		return err
	}
	if p.Ordinal <= 0 {
		return fmt.Errorf("ordinal %d is not positive", p.Ordinal)
	}
	pv := p.Provenance
	fields := []struct{ name, v string }{
		{"role", string(pv.Role)}, {"prompt version", pv.PromptVersion}, {"model", pv.Model}, {"provider", pv.Provider},
	}
	for _, f := range fields {
		if f.v == "" {
			return fmt.Errorf("empty %s", f.name)
		}
		if err := checkText(f.name, f.v); err != nil {
			return err
		}
	}
	return nil
}

// SetAttemptSandbox records the sandbox an attempt runs in, before any
// agent step, so cleanup can name it after a crash (RL-3). It writes only
// when the attempt has no sandbox yet; recording the same ID again is a
// no-op, and a different ID is ErrSandboxMismatch. An unknown attempt is
// ErrNotFound.
func SetAttemptSandbox(ctx context.Context, db DBTX, attemptID, sandboxID string) error {
	if err := checkUUID("attempt ID", attemptID); err != nil {
		return fmt.Errorf("recording sandbox: %w", err)
	}
	if sandboxID == "" {
		return fmt.Errorf("recording sandbox of attempt %s: empty sandbox ID", attemptID)
	}
	if err := checkText("sandbox ID", sandboxID); err != nil {
		return fmt.Errorf("recording sandbox of attempt %s: %w", attemptID, err)
	}
	const set = `UPDATE attempts SET sandbox_id = $2, updated_at = now() WHERE id = $1 AND sandbox_id IS NULL`
	tag, err := db.Exec(ctx, set, attemptID, sandboxID)
	if err != nil {
		return fmt.Errorf("recording sandbox %s on attempt %s: %w", sandboxID, attemptID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := readAttemptText(ctx, db, attemptID, "sandbox_id")
	if err != nil {
		return fmt.Errorf("recording sandbox %s: %w", sandboxID, err)
	}
	if stored != sandboxID {
		return fmt.Errorf("attempt %s records sandbox %q, not %q: %w", attemptID, stored, sandboxID, ErrSandboxMismatch)
	}
	return nil
}

// MarkSandboxReleased records that the attempt's sandbox is gone. The
// first call stamps sandbox_released_at with the database clock; later
// calls leave it alone. An unknown attempt is ErrNotFound.
func MarkSandboxReleased(ctx context.Context, db DBTX, attemptID string) error {
	if err := checkUUID("attempt ID", attemptID); err != nil {
		return fmt.Errorf("marking sandbox released: %w", err)
	}
	const mark = `UPDATE attempts SET sandbox_released_at = now(), updated_at = now()
		WHERE id = $1 AND sandbox_released_at IS NULL`
	tag, err := db.Exec(ctx, mark, attemptID)
	if err != nil {
		return fmt.Errorf("marking sandbox of attempt %s released: %w", attemptID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := readAttemptText(ctx, db, attemptID, "id::text"); err != nil {
		return fmt.Errorf("marking sandbox released: %w", err)
	}
	return nil
}

// SetArtifactHash records the hash of the attempt's frozen artifact
// (FR-SEC27). It writes only when no hash is recorded; the same hash again
// is a no-op, and a different one is ErrArtifactMismatch, refused here
// before migration 00007's write-once trigger would abort the
// transaction. The hash must be lowercase hex SHA-256, gate.HashArtifact's
// output. An unknown attempt is ErrNotFound.
func SetArtifactHash(ctx context.Context, db DBTX, attemptID, hash string) error {
	if err := checkUUID("attempt ID", attemptID); err != nil {
		return fmt.Errorf("recording artifact hash: %w", err)
	}
	if !artifactHashRE.MatchString(hash) {
		return fmt.Errorf("recording artifact hash of attempt %s: %q is not lowercase hex SHA-256", attemptID, hash)
	}
	const set = `UPDATE attempts SET artifact_hash = $2, updated_at = now() WHERE id = $1 AND artifact_hash IS NULL`
	tag, err := db.Exec(ctx, set, attemptID, hash)
	if err != nil {
		return fmt.Errorf("recording artifact hash of attempt %s: %w", attemptID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := readAttemptText(ctx, db, attemptID, "artifact_hash")
	if err != nil {
		return fmt.Errorf("recording artifact hash: %w", err)
	}
	if stored != hash {
		return fmt.Errorf("attempt %s records artifact %s, not %s: %w", attemptID, stored, hash, ErrArtifactMismatch)
	}
	return nil
}

// readAttemptText reads one text column of an attempt, "" for NULL, or
// ErrNotFound. column is a constant from this file, never input.
func readAttemptText(ctx context.Context, db DBTX, attemptID, column string) (string, error) {
	var v string
	err := db.QueryRow(ctx, `SELECT coalesce(`+column+`, '') FROM attempts WHERE id = $1`, attemptID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("attempt %s: %w", attemptID, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("reading attempt %s: %w", attemptID, err)
	}
	return v, nil
}

// FinishParams describes how an attempt ended.
type FinishParams struct {
	AttemptID string

	// Revision is the attempt's revision as last read, in state running.
	Revision int

	// To is the terminal state. Reason is required for failed and errored
	// and refused for passed.
	To     contracts.AttemptState
	Reason contracts.FailureReason

	// Actor is who caused the change, for the audit log; see ActorWorkflow.
	Actor string

	Usage contracts.Usage

	// Model and Provider are what actually answered, which a fallback can
	// make differ from what CreateAttempt recorded. Empty keeps the
	// recorded value, for an attempt that ended before any model call.
	Model    string
	Provider string

	// Report is the agent's completion report, or nil when it made none.
	Report *contracts.CompletionReport
}

// maxCostUSD is the largest value attempts.cost_usd, numeric(12, 6), holds.
const maxCostUSD = 999_999.999999

// FinishAttempt moves a running attempt to its terminal state and records
// its usage, actual model and provider, report and finish time, in tx.
//
// Transition runs first, and the columns are written only when it
// succeeded. A retry after a committed success therefore gets
// ErrAlreadyApplied from Transition and writes nothing, so the first
// call's usage and report stand (RL-2), and a stale call gets a
// WrongStateError and writes nothing either. Writing the columns first
// would need a second compare-and-set to be as safe. Every input is
// checked before any SQL runs, so the only failures after the transition
// are database faults, and the caller rolls tx back on any error that is
// not ErrAlreadyApplied.
func FinishAttempt(ctx context.Context, tx pgx.Tx, p FinishParams) error {
	report, err := checkFinish(p)
	if err != nil {
		return fmt.Errorf("finishing attempt %s: %w", p.AttemptID, err)
	}
	req := TransitionRequest{
		Kind: contracts.EntityAttempt, ID: p.AttemptID, Revision: p.Revision,
		From: string(contracts.AttemptRunning), To: string(p.To),
		Actor: p.Actor, Reason: string(p.Reason),
	}
	if err := Transition(ctx, tx, req); err != nil {
		return fmt.Errorf("finishing attempt %s: %w", p.AttemptID, err)
	}
	// Conditional on the state and revision Transition just wrote, so the
	// write lands on the change this call made and on nothing else.
	const record = `UPDATE attempts SET
			input_tokens = $2, output_tokens = $3, cost_usd = $4, steps = $5,
			model = coalesce(NULLIF($6, ''), model), provider = coalesce(NULLIF($7, ''), provider),
			report = $8::jsonb, finished_at = now(), updated_at = now()
		WHERE id = $1 AND state = $9 AND revision = $10`
	u := p.Usage
	tag, err := tx.Exec(ctx, record, p.AttemptID, u.InputTokens, u.OutputTokens, u.CostUSD, u.Steps,
		p.Model, p.Provider, report, string(p.To), p.Revision+1)
	if err != nil {
		return fmt.Errorf("recording outcome of attempt %s: %w", p.AttemptID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("recording outcome of attempt %s: %d rows matched after its transition", p.AttemptID, tag.RowsAffected())
	}
	return nil
}

// checkFinish refuses what Transition or the table would refuse after the
// state had already changed, and returns the report as jsonb text, or nil.
func checkFinish(p FinishParams) (*string, error) {
	if err := checkUUID("attempt ID", p.AttemptID); err != nil {
		return nil, err
	}
	if err := validate(TransitionRequest{
		Kind: contracts.EntityAttempt, ID: p.AttemptID, Revision: p.Revision,
		From: string(contracts.AttemptRunning), To: string(p.To), Actor: p.Actor,
	}); err != nil {
		return nil, err
	}
	if err := checkFinishReason(p.To, p.Reason); err != nil {
		return nil, err
	}
	if err := checkUsage(p.Usage); err != nil {
		return nil, err
	}
	for _, f := range []struct{ name, v string }{{"actor", p.Actor}, {"model", p.Model}, {"provider", p.Provider}} {
		if err := checkText(f.name, f.v); err != nil {
			return nil, err
		}
	}
	if p.Report == nil {
		return nil, nil
	}
	doc, err := json.Marshal(p.Report)
	if err != nil {
		return nil, fmt.Errorf("encoding report: %w", err)
	}
	if err := checkJSONB("report", doc); err != nil {
		return nil, err
	}
	s := string(doc)
	return &s, nil
}

func checkFinishReason(to contracts.AttemptState, reason contracts.FailureReason) error {
	switch {
	case to == contracts.AttemptPassed && reason != "":
		return fmt.Errorf("a passed attempt has no failure reason, got %q", reason)
	case to != contracts.AttemptPassed && reason == "":
		return fmt.Errorf("a %s attempt needs a failure reason", to)
	}
	return checkText("reason", string(reason))
}

func checkUsage(u contracts.Usage) error {
	switch {
	case u.InputTokens < 0 || u.OutputTokens < 0 || u.Steps < 0:
		return fmt.Errorf("negative usage: %d input tokens, %d output tokens, %d steps", u.InputTokens, u.OutputTokens, u.Steps)
	case math.IsNaN(u.CostUSD) || u.CostUSD < 0 || u.CostUSD > maxCostUSD:
		return fmt.Errorf("cost %v is outside [0, %v]", u.CostUSD, maxCostUSD)
	}
	return nil
}

// UnreleasedAttempt is an attempt whose sandbox is recorded and not yet
// released. SandboxID is carried beside the contract record, which has no
// field for it, because it is what the reaper hands to Release.
type UnreleasedAttempt struct {
	Attempt   contracts.Attempt
	SandboxID string
}

// UnreleasedAttempts returns the attempts that record a sandbox, have not
// marked it released, and started more than olderThan ago on the database
// clock, oldest first. It is the orphan reaper's query and uses the
// attempts_sandbox_unreleased partial index. olderThan should exceed the
// longest an attempt can run, or the reaper would release a sandbox still
// in use; the state is returned so the reaper can also refuse running
// attempts.
func UnreleasedAttempts(ctx context.Context, db Querier, olderThan time.Duration) ([]UnreleasedAttempt, error) {
	if olderThan < 0 {
		return nil, fmt.Errorf("listing unreleased sandboxes: negative age %s", olderThan)
	}
	const query = `SELECT ` + attemptColumns + `, sandbox_id FROM attempts
		WHERE sandbox_released_at IS NULL AND sandbox_id IS NOT NULL
		  AND started_at < now() - $1::bigint * interval '1 microsecond'
		ORDER BY started_at, id`
	rows, err := db.Query(ctx, query, olderThan.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("listing unreleased sandboxes older than %s: %w", olderThan, err)
	}
	defer rows.Close()
	var out []UnreleasedAttempt
	for rows.Next() {
		var u UnreleasedAttempt
		if u.Attempt, err = scanAttempt(rows, &u.SandboxID); err != nil {
			return nil, fmt.Errorf("listing unreleased sandboxes: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing unreleased sandboxes: %w", err)
	}
	return out, nil
}
