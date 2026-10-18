package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

func toolCall(attempt string, step int) agent.ToolCallRecord {
	return agent.ToolCallRecord{
		AttemptID:    attempt,
		SessionKey:   "wf-12/activity-3/1",
		Step:         step,
		CallID:       "toolu_01",
		Tool:         contracts.ToolName("read_file"),
		ArgumentsRaw: []byte(`{"path":"src/main.go"}`),
		ExitCode:     0,
		Duration:     1500 * time.Millisecond,
		ArtifactKey:  "0f6b1c2a-5d1e-4c8e-9a4b-1f2e3d4c5b6a/" + strings.Repeat("ab", 32),
		Error:        "",
		Rejected:     false,
		Truncated:    true,
		OccurredAt:   time.Date(2026, 10, 18, 12, 0, 0, 123456789, time.UTC),
	}
}

func countToolCalls(t *testing.T, ctx context.Context, tx pgx.Tx, attempt string) int {
	t.Helper()
	return countRows(t, ctx, tx, `SELECT count(*) FROM tool_calls WHERE attempt_id = $1`, attempt)
}

// TestRecordToolCallIsIdempotent replays a record the way a retried
// activity does. The second write must succeed and add nothing, as the
// application role, whose only rights on tool_calls are SELECT and INSERT.
func TestRecordToolCallIsIdempotent(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	rec := store.NewToolCalls(tx)

	for i := range 2 {
		if err := rec.Record(ctx, toolCall(s.attempt, 1)); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
	}
	if n := countToolCalls(t, ctx, tx, s.attempt); n != 1 {
		t.Fatalf("%d rows for one call, want 1", n)
	}
}

// TestToolCallConflictMismatchErrors: a second record under the same
// (attempt, session, step) that differs in any one column is refused, so a
// retry can never hide a different call behind the first. Differences the
// table cannot hold, below a millisecond of duration or a microsecond of
// time, are the same call.
func TestToolCallConflictMismatchErrors(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	rec := store.NewToolCalls(tx)
	if err := rec.Record(ctx, toolCall(s.attempt, 1)); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// column names the column the change should be refused for; empty
	// means the change is not a conflict.
	cases := []struct {
		name   string
		change func(*agent.ToolCallRecord)
		column string
	}{
		{"call ID", func(r *agent.ToolCallRecord) { r.CallID = "toolu_02" }, "call_id"},
		{"tool", func(r *agent.ToolCallRecord) { r.Tool = "write_file" }, "tool"},
		{"raw arguments", func(r *agent.ToolCallRecord) { r.ArgumentsRaw = []byte(`{"path":"src/other.go"}`) }, "arguments_raw"},
		{"arguments no longer JSON", func(r *agent.ToolCallRecord) { r.ArgumentsRaw = []byte(`{"path":`) }, "arguments_raw arguments"},
		{"exit code", func(r *agent.ToolCallRecord) { r.ExitCode = 1 }, "exit_code"},
		{"duration", func(r *agent.ToolCallRecord) { r.Duration += time.Millisecond }, "duration_ms"},
		{"artifact key", func(r *agent.ToolCallRecord) { r.ArtifactKey = "" }, "artifact_key"},
		{"error", func(r *agent.ToolCallRecord) { r.Error = "invalid_path" }, "error"},
		{"rejected", func(r *agent.ToolCallRecord) { r.Rejected = true }, "rejected"},
		{"truncated", func(r *agent.ToolCallRecord) { r.Truncated = false }, "truncated"},
		{"occurred at", func(r *agent.ToolCallRecord) { r.OccurredAt = r.OccurredAt.Add(time.Microsecond) }, "occurred_at"},
		{"duration below a millisecond", func(r *agent.ToolCallRecord) { r.Duration += 400 * time.Microsecond }, ""},
		{"occurred at below a microsecond", func(r *agent.ToolCallRecord) { r.OccurredAt = r.OccurredAt.Add(200) }, ""},
		{"occurred at in another zone", func(r *agent.ToolCallRecord) {
			r.OccurredAt = r.OccurredAt.In(time.FixedZone("CDT", -5*3600))
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := toolCall(s.attempt, 1)
			tc.change(&r)
			err := rec.Record(ctx, r)
			if tc.column == "" {
				if err != nil {
					t.Fatalf("same call refused: %v", err)
				}
				return
			}
			if !errors.Is(err, store.ErrToolCallConflict) {
				t.Fatalf("got %v, want ErrToolCallConflict", err)
			}
			if !strings.Contains(err.Error(), "["+tc.column+"]") {
				t.Errorf("error %q does not name exactly the column %s", err, tc.column)
			}
			if n := countToolCalls(t, ctx, tx, s.attempt); n != 1 {
				t.Fatalf("%d rows after a second write, want 1", n)
			}
		})
	}
}

// TestToolCallArgumentsStoredRaw: whatever the model sent is kept byte for
// byte, and the jsonb column is filled only when Postgres accepts the bytes
// as jsonb. Arguments it refuses never make the audit write fail.
func TestToolCallArgumentsStoredRaw(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	rec := store.NewToolCalls(tx)

	cases := []struct {
		name     string
		raw      []byte
		wantJSON bool
	}{
		{"object", []byte(`{"path":"a.go","start_line":3}`), true},
		{"truncated object", []byte(`{"path":`), false},
		{"not JSON at all", []byte(`read a.go please`), false},
		{"invalid UTF-8 in a string", []byte("{\"path\":\"\xff\xfe\"}"), false},
		{"NUL escape", []byte(`{"path":"a\u0000.go"}`), false},
		{"number beyond numeric", []byte(`{"n":1e1000000}`), false},
		// As deep as json.Valid allows: Postgres must not fail on it.
		{"deeply nested", []byte(strings.Repeat("[", 10000) + strings.Repeat("]", 10000)), true},
		{"nested beyond json.Valid", []byte(strings.Repeat("[", 10001) + strings.Repeat("]", 10001)), false},
		{"empty", []byte{}, false},
		{"nil", nil, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := toolCall(s.attempt, i+1)
			r.ArgumentsRaw = tc.raw
			if err := rec.Record(ctx, r); err != nil {
				t.Fatalf("record: %v", err)
			}
			var raw []byte
			var hasJSON bool
			err := tx.QueryRow(ctx, `SELECT arguments_raw, arguments IS NOT NULL FROM tool_calls
				WHERE attempt_id = $1 AND step = $2`, s.attempt, i+1).Scan(&raw, &hasJSON)
			if err != nil {
				t.Fatalf("reading back: %v", err)
			}
			if string(raw) != string(tc.raw) {
				t.Errorf("arguments_raw = %q, want %q", raw, tc.raw)
			}
			if hasJSON != tc.wantJSON {
				t.Errorf("arguments set = %v, want %v", hasJSON, tc.wantJSON)
			}
		})
	}
}

// TestRecordRefusesIncompleteRecords: a record the table would reject is
// refused before any SQL runs, so it cannot abort a caller's transaction.
func TestRecordRefusesIncompleteRecords(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	rec := store.NewToolCalls(tx)

	cases := []struct {
		name   string
		change func(*agent.ToolCallRecord)
	}{
		{"attempt ID not a UUID", func(r *agent.ToolCallRecord) { r.AttemptID = "attempt-1" }},
		{"empty session key", func(r *agent.ToolCallRecord) { r.SessionKey = "" }},
		{"step zero", func(r *agent.ToolCallRecord) { r.Step = 0 }},
		{"negative duration", func(r *agent.ToolCallRecord) { r.Duration = -time.Second }},
		{"no occurred-at time", func(r *agent.ToolCallRecord) { r.OccurredAt = time.Time{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := toolCall(s.attempt, 1)
			tc.change(&r)
			if err := rec.Record(ctx, r); err == nil {
				t.Fatal("record accepted")
			}
		})
	}
	// The transaction is still usable: nothing reached the database.
	if err := rec.Record(ctx, toolCall(s.attempt, 1)); err != nil {
		t.Fatalf("valid record after refusals: %v", err)
	}
}

// TestRejectionCount counts only the rejected calls of the attempt asked
// about, across its sessions.
func TestRejectionCount(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	other, err := insertInState(ctx, tx, s, contracts.EntityAttempt, "running")
	if err != nil {
		t.Fatalf("seeding second attempt: %v", err)
	}
	rec := store.NewToolCalls(tx)

	write := func(attempt, session string, step int, rejected bool) {
		t.Helper()
		r := toolCall(attempt, step)
		r.SessionKey = session
		r.Rejected = rejected
		if err := rec.Record(ctx, r); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	write(s.attempt, "session-1", 1, true)
	write(s.attempt, "session-1", 2, false)
	write(s.attempt, "session-2", 1, true)
	write(other, "session-3", 1, true)

	cases := []struct {
		name    string
		attempt string
		want    int
	}{
		{"attempt with two rejections", s.attempt, 2},
		{"other attempt", other, 1},
		{"attempt with no calls", "00000000-0000-4000-8000-000000000000", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rec.RejectionCount(ctx, tc.attempt)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("RejectionCount = %d, want %d", got, tc.want)
			}
		})
	}
	if _, err := rec.RejectionCount(ctx, "attempt-1"); !errors.Is(err, store.ErrInvalidID) {
		t.Fatalf("non-UUID attempt: got %v, want ErrInvalidID", err)
	}
}
