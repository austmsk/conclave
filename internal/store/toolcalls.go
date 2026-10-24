package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/austmsk/conclave/internal/contracts"
)

// ErrToolCallConflict means a tool call was recorded under a key that
// already holds a different call. A retried write of the same call is
// absorbed; this is the case where the two disagree, which only a bug or a
// reused session key can cause.
var ErrToolCallConflict = errors.New("a different tool call is already recorded under this key")

// ToolCalls writes the tool-call audit (FR-SEC24) to tool_calls, which is
// append-only to the application role. It implements agent.Recorder; the
// assertion lives in toolcalls_agent_test.go so store does not import agent.
type ToolCalls struct {
	db DBTX
}

// NewToolCalls returns a recorder over db, which should connect as the
// application role so the audit cannot be rewritten through it (threat
// T6). In production db is the pool.
func NewToolCalls(db DBTX) *ToolCalls {
	return &ToolCalls{db: db}
}

// toolCallRow is a record as the table holds it: duration in whole
// milliseconds, time at microsecond precision, and empty optional text
// standing for NULL. Comparing two rows compares what Postgres would store
// for each.
type toolCallRow struct {
	callID      string
	tool        string
	argsRaw     []byte
	hasArgs     bool
	exitCode    int
	durationMS  int64
	artifactKey string
	errText     string
	rejected    bool
	truncated   bool
	occurredAt  time.Time
}

func rowOf(rec contracts.ToolCallRecord) toolCallRow {
	// pgx sends a nil slice as NULL, and arguments_raw is NOT NULL: no
	// arguments at all is stored as zero bytes.
	raw := rec.ArgumentsRaw
	if raw == nil {
		raw = []byte{}
	}
	return toolCallRow{
		callID:      rec.CallID,
		tool:        string(rec.Tool),
		argsRaw:     raw,
		exitCode:    rec.ExitCode,
		durationMS:  rec.Duration.Milliseconds(),
		artifactKey: rec.ArtifactKey,
		errText:     rec.Error,
		rejected:    rec.Rejected,
		truncated:   rec.Truncated,
		occurredAt:  rec.OccurredAt.Truncate(time.Microsecond),
	}
}

// Record inserts rec unless a row already holds its (AttemptID,
// SessionKey, Step), then reads the stored row back and compares every
// column. ON CONFLICT DO NOTHING needs only INSERT, so a retried write
// succeeds and the call is recorded once; a stored row that differs from
// rec is ErrToolCallConflict, so a retry can never hide a second, different
// call behind the first.
//
// The arguments column is derived from ArgumentsRaw: set when the bytes
// are JSON that Postgres accepts as jsonb, null otherwise. Malformed
// arguments never make the write fail; the raw bytes are kept either way.
func (r *ToolCalls) Record(ctx context.Context, rec contracts.ToolCallRecord) error {
	key := fmt.Sprintf("%s/%s/%d", rec.AttemptID, rec.SessionKey, rec.Step)
	if err := checkToolCall(rec); err != nil {
		return fmt.Errorf("recording tool call %s: %w", key, err)
	}
	want := rowOf(rec)
	args, err := r.jsonbArguments(ctx, want.argsRaw)
	if err != nil {
		return fmt.Errorf("recording tool call %s: checking arguments: %w", key, err)
	}
	want.hasArgs = args != nil
	if err := r.insert(ctx, rec, want, args); err != nil {
		return fmt.Errorf("recording tool call %s: %w", key, err)
	}
	got, err := r.stored(ctx, rec)
	if err != nil {
		return fmt.Errorf("recording tool call %s: reading it back: %w", key, err)
	}
	if diff := differingColumns(want, got); len(diff) > 0 {
		return fmt.Errorf("recording tool call %s: %w: %v differ", key, ErrToolCallConflict, diff)
	}
	return nil
}

// RejectionCount returns how many of an attempt's tool calls were refused
// on security grounds, across all its sessions.
func (r *ToolCalls) RejectionCount(ctx context.Context, attemptID string) (int, error) {
	if err := checkUUID("attempt ID", attemptID); err != nil {
		return 0, fmt.Errorf("counting rejected tool calls: %w", err)
	}
	var n int
	err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM tool_calls WHERE attempt_id = $1 AND rejected`, attemptID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting rejected tool calls for attempt %s: %w", attemptID, err)
	}
	return n, nil
}

// checkToolCall refuses a record the table would reject, before any SQL
// runs: an error inside a caller's transaction would abort it.
func checkToolCall(rec contracts.ToolCallRecord) error {
	if err := checkUUID("attempt ID", rec.AttemptID); err != nil {
		return err
	}
	switch {
	case rec.SessionKey == "":
		return errors.New("empty session key")
	case rec.Step < 1:
		return fmt.Errorf("step %d: steps number from 1", rec.Step)
	case rec.Duration < 0:
		return fmt.Errorf("negative duration %s", rec.Duration)
	case rec.OccurredAt.IsZero():
		return errors.New("no occurred-at time: the worker must stamp it")
	}
	return nil
}

// jsonbArguments returns raw as text for the jsonb column, or nil when it
// should be null. json.Valid alone is not enough: it accepts invalid UTF-8,
// which a text parameter cannot carry, and Postgres refuses some valid
// JSON, such as a \u0000 escape or a number beyond numeric's range. So the
// last word is Postgres's own, asked with pg_input_is_valid, which reports
// bad input without raising an error. A separate query rather than a CASE
// in the insert, because the planner may fold a cast of a constant
// parameter before the CASE is ever evaluated.
func (r *ToolCalls) jsonbArguments(ctx context.Context, raw []byte) (*string, error) {
	if !json.Valid(raw) || !utf8.Valid(raw) {
		return nil, nil
	}
	s := string(raw)
	var ok bool
	if err := r.db.QueryRow(ctx, `SELECT pg_input_is_valid($1, 'jsonb')`, s).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &s, nil
}

func (r *ToolCalls) insert(ctx context.Context, rec contracts.ToolCallRecord, row toolCallRow, args *string) error {
	const insert = `INSERT INTO tool_calls
		(attempt_id, session_key, step, call_id, tool, arguments_raw, arguments,
		 exit_code, duration_ms, artifact_key, error, rejected, truncated, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::text::jsonb,
		        $8, $9, NULLIF($10, ''), NULLIF($11, ''), $12, $13, $14)
		ON CONFLICT (attempt_id, session_key, step) DO NOTHING`
	_, err := r.db.Exec(ctx, insert,
		rec.AttemptID, rec.SessionKey, rec.Step, row.callID, row.tool, row.argsRaw, args,
		row.exitCode, row.durationMS, row.artifactKey, row.errText, row.rejected, row.truncated, row.occurredAt)
	return err
}

func (r *ToolCalls) stored(ctx context.Context, rec contracts.ToolCallRecord) (toolCallRow, error) {
	const query = `SELECT call_id, tool, arguments_raw, arguments IS NOT NULL,
		exit_code, duration_ms, coalesce(artifact_key, ''), coalesce(error, ''),
		rejected, truncated, occurred_at
		FROM tool_calls WHERE attempt_id = $1 AND session_key = $2 AND step = $3`
	var g toolCallRow
	err := r.db.QueryRow(ctx, query, rec.AttemptID, rec.SessionKey, rec.Step).Scan(
		&g.callID, &g.tool, &g.argsRaw, &g.hasArgs,
		&g.exitCode, &g.durationMS, &g.artifactKey, &g.errText,
		&g.rejected, &g.truncated, &g.occurredAt)
	return g, err
}

// differingColumns names the columns in which two rows differ. The jsonb
// column is compared by presence only: its content is a function of
// arguments_raw, which is compared byte for byte.
func differingColumns(want, got toolCallRow) []string {
	cols := []struct {
		name string
		same bool
	}{
		{"call_id", want.callID == got.callID},
		{"tool", want.tool == got.tool},
		{"arguments_raw", bytes.Equal(want.argsRaw, got.argsRaw)},
		{"arguments", want.hasArgs == got.hasArgs},
		{"exit_code", want.exitCode == got.exitCode},
		{"duration_ms", want.durationMS == got.durationMS},
		{"artifact_key", want.artifactKey == got.artifactKey},
		{"error", want.errText == got.errText},
		{"rejected", want.rejected == got.rejected},
		{"truncated", want.truncated == got.truncated},
		{"occurred_at", want.occurredAt.Equal(got.occurredAt)},
	}
	var diff []string
	for _, c := range cols {
		if !c.same {
			diff = append(diff, c.name)
		}
	}
	return diff
}
