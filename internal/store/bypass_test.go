package store_test

import (
	"fmt"
	"testing"

	"github.com/austmsk/conclave/internal/store"
)

// TestAuditTriggerResistsBypass attempts, as the application role, each way
// of changing a state column without leaving an honest audit row: skipping
// the actor, disabling the trigger, switching the session to replica mode
// so triggers do not fire, and creating a temp table to shadow the audit
// table. Every one must be refused by the database, not by convention
// (FR-SEC10).
func TestAuditTriggerResistsBypass(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"update without actor", `UPDATE attempts SET state = 'passed' WHERE id = '` + s.attempt + `'`,
			store.SQLStateUnattributedStateChange},
		{"disable the trigger", `ALTER TABLE attempts DISABLE TRIGGER attempts_state_transition`, permissionDenied},
		{"replica replication role", `SET LOCAL session_replication_role = 'replica'`, permissionDenied},
		{"shadow the audit table", `CREATE TEMP TABLE state_transitions (LIKE public.state_transitions INCLUDING ALL)`,
			permissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := savepoint(t, ctx, tx)
			_, err := sp.Exec(ctx, tc.sql)
			if got := sqlState(err); got != tc.want {
				t.Fatalf("got %v (SQLSTATE %q), want SQLSTATE %s", err, got, tc.want)
			}
		})
	}

	if state, _ := stateOf(t, ctx, tx, "attempts", s.attempt); state != "running" {
		t.Errorf("attempt state = %q after refused updates, want running", state)
	}
	if n := len(audits(t, ctx, tx, s.attempt)); n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}
	// Updates that leave state alone are unaffected.
	if _, err := tx.Exec(ctx, `UPDATE attempts SET steps = 3 WHERE id = $1`, s.attempt); err != nil {
		t.Errorf("non-state update: %v", err)
	}
}

// TestTempTableCannotShadowAuditTable checks the trigger's own defence,
// independent of the TEMP grant: a role that can create temp tables (here
// the owner) still cannot divert audit rows, because the function's
// search_path is pinned and its target is schema-qualified.
func TestTempTableCannotShadowAuditTable(t *testing.T) {
	ctx := requireDB(t)
	tx, err := pool.Begin(ctx) // owner: has TEMP
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	s := seed(t, ctx, tx)

	steps := []string{
		`CREATE TEMP TABLE state_transitions (LIKE public.state_transitions INCLUDING ALL)`,
		`SELECT set_config('conclave.actor', 'attacker', true)`,
		`UPDATE attempts SET state = 'passed' WHERE id = '` + s.attempt + `'`,
	}
	for _, q := range steps {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	if got := audits(t, ctx, tx, s.attempt); len(got) != 1 || got[0].actor != "attacker" {
		t.Errorf("public.state_transitions rows = %+v, want one row by attacker", got)
	}
	var diverted int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_temp.state_transitions`).Scan(&diverted); err != nil {
		t.Fatalf("counting temp rows: %v", err)
	}
	if diverted != 0 {
		t.Errorf("temp table received %d audit rows, want 0", diverted)
	}
}

// TestSessionScopedActorIsStillAudited documents the limit of the control.
// An application that sets conclave.actor at session scope can attribute a
// direct update to any string it likes; what it cannot do is avoid the
// audit row, or forge db_user, which the trigger takes from the
// authenticated login.
func TestSessionScopedActorIsStillAudited(t *testing.T) {
	ctx := requireDB(t)
	conn := ownerConn(t, ctx)
	for _, q := range []string{
		`SET ROLE conclave_app`,
		`SELECT set_config('conclave.actor', 'forged', false)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	s := seed(t, ctx, tx)

	if _, err := tx.Exec(ctx, `UPDATE attempts SET state = 'passed' WHERE id = $1`, s.attempt); err != nil {
		t.Fatalf("direct update with session-scoped actor: %v", err)
	}
	want := []audit{{kind: "attempt", from: "running", to: "passed", actor: "forged",
		dbUser: sessionUser(t, ctx, tx), revision: 1}}
	if got := audits(t, ctx, tx, s.attempt); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audit rows = %+v, want %+v", got, want)
	}
	if want[0].dbUser == "conclave_app" {
		t.Error("db_user must be the login, not the SET ROLE target")
	}
}
