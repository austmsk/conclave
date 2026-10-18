package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// asAppRole runs fn inside a savepoint with the application role active, so
// the privilege checks are the ones a worker would hit. The savepoint is
// always rolled back.
func asAppRole(t *testing.T, ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	t.Helper()
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	defer func() { _ = sp.Rollback(ctx) }()
	if _, err := sp.Exec(ctx, `SET LOCAL ROLE conclave_app`); err != nil {
		t.Fatalf("set role: %v", err)
	}
	return fn(sp)
}

func seedAccounting(t *testing.T, ctx context.Context, tx pgx.Tx, s ids) {
	t.Helper()
	_, err := tx.Exec(ctx, `INSERT INTO cost_events
		(idempotency_key, repo_id, attempt_id, role, provider, model, input_tokens, output_tokens, cost_usd)
		VALUES ('req-1', $1, $2, 'implementer', 'anthropic', 'claude-fable-5-1', 10, 5, 0.001)`, s.repo, s.attempt)
	if err != nil {
		t.Fatalf("seeding cost event: %v", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO state_transitions
		(entity_kind, entity_id, from_state, to_state, actor, db_user, revision)
		VALUES ('task', $1, 'running', 'done', 'test', 'test', 1)`, s.task)
	if err != nil {
		t.Fatalf("seeding state transition: %v", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO tool_calls
		(attempt_id, session_key, step, call_id, tool, arguments_raw, exit_code, duration_ms,
		 rejected, truncated, occurred_at)
		VALUES ($1, 'session-1', 1, 'toolu_01', 'read_file', '{}', 0, 5, false, false, now())`, s.attempt)
	if err != nil {
		t.Fatalf("seeding tool call: %v", err)
	}
}

// TestAppRoleCannotAlterAppendOnlyRows attempts the attack in threat T6: a
// compromised worker rewriting cost or audit history. Every mutation of an
// append-only table must be refused at the database, while the same role
// keeps its ordinary access, so the refusals are not an artefact of the role
// having no privileges at all.
func TestAppRoleCannotAlterAppendOnlyRows(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	seedAccounting(t, ctx, tx, s)

	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"insert cost event", `INSERT INTO cost_events
			(idempotency_key, repo_id, role, provider, model, input_tokens, output_tokens, cost_usd)
			VALUES ('req-2', '` + s.repo + `', 'implementer', 'anthropic', 'm', 1, 1, 0)`, ""},
		{"update cost event", `UPDATE cost_events SET cost_usd = 0`, permissionDenied},
		{"delete cost event", `DELETE FROM cost_events`, permissionDenied},
		{"truncate cost events", `TRUNCATE cost_events`, permissionDenied},
		{"insert state transition", `INSERT INTO state_transitions
			(entity_kind, entity_id, from_state, to_state, actor, db_user, revision)
			VALUES ('task', '` + s.task + `', 'running', 'done', 'test', 'test', 2)`, ""},
		{"update state transition", `UPDATE state_transitions SET actor = 'someone else'`, permissionDenied},
		{"delete state transition", `DELETE FROM state_transitions`, permissionDenied},
		{"truncate state transitions", `TRUNCATE state_transitions`, permissionDenied},
		{"insert tool call", `INSERT INTO tool_calls
			(attempt_id, session_key, step, call_id, tool, arguments_raw, exit_code, duration_ms,
			 rejected, truncated, occurred_at)
			VALUES ('` + s.attempt + `', 'session-1', 2, 'toolu_02', 'read_file', '{}', 0, 5, false, false, now())`, ""},
		{"update tool call", `UPDATE tool_calls SET rejected = false, error = NULL`, permissionDenied},
		{"delete tool call", `DELETE FROM tool_calls`, permissionDenied},
		{"truncate tool calls", `TRUNCATE tool_calls`, permissionDenied},
		{"update ordinary table", `UPDATE repos SET default_branch = 'trunk'`, ""},
		{"delete from ordinary table", `DELETE FROM repos`, permissionDenied},
		{"drop append-only table", `DROP TABLE cost_events`, permissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := asAppRole(t, ctx, tx, func(sp pgx.Tx) error {
				_, err := sp.Exec(ctx, tc.sql)
				return err
			})
			if got := sqlState(err); got != tc.want {
				t.Fatalf("got error %v (SQLSTATE %q), want SQLSTATE %q", err, got, tc.want)
			}
		})
	}
}
