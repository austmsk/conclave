package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/austmsk/conclave/internal/store"
)

// loginSeq makes each test login's name unique; roles are cluster-wide.
var loginSeq atomic.Int64

// asLogin creates a LOGIN role that is a member of each of memberOf, and
// connects as it, as a deployed worker or CLI would. The role and the
// connection are removed when the test ends. The password is a canary,
// never a real one.
func asLogin(t *testing.T, ctx context.Context, memberOf ...string) (*pgx.Conn, string) {
	t.Helper()
	name := fmt.Sprintf("conclave_test_login_%d", loginSeq.Add(1))
	const password = "canary_login_test_only"
	create := fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, name, password)
	if len(memberOf) > 0 {
		create += " IN ROLE " + strings.Join(memberOf, ", ")
	}
	if _, err := pool.Exec(ctx, create); err != nil {
		t.Fatalf("creating login %s: %v", name, err)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = name, password
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting as %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = conn.Close(context.Background())
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+name)
	})
	return conn, name
}

// ownerLogin is the name the test database's owner logs in as.
func ownerLogin(t *testing.T) string {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.User
}

// TestCurrentLoginChecks connects as each kind of login and checks which
// process each may run: the owner, a superuser, or a member of the owner
// runs neither; an app login runs only the worker; an operator login runs
// only the CLI.
func TestCurrentLoginChecks(t *testing.T) {
	ctx := requireDB(t)
	tests := []struct {
		name                 string
		memberOf             []string
		owner                bool
		wantWorker, wantCLI  bool
		wantWorkerMentioning string
	}{
		{name: "owner", owner: true, wantWorkerMentioning: "superuser"},
		{name: "member of the owner", memberOf: []string{"conclave_app", ownerLogin(t)}, wantWorkerMentioning: "owns the schema"},
		{name: "app", memberOf: []string{store.RoleApp}, wantWorker: true},
		{name: "operator", memberOf: []string{store.RoleOperator}, wantCLI: true, wantWorkerMentioning: "could lift a halt"},
		{name: "no role", wantWorkerMentioning: "not a conclave_app member"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var db store.DBTX = pool
			if !tt.owner {
				db, _ = asLogin(t, ctx, tt.memberOf...)
			}
			l, err := store.CurrentLogin(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			werr, cerr := l.CheckWorker(), l.CheckOperator()
			if (werr == nil) != tt.wantWorker || (cerr == nil) != tt.wantCLI {
				t.Fatalf("%+v: CheckWorker = %v, CheckOperator = %v; want worker %t, CLI %t", l, werr, cerr, tt.wantWorker, tt.wantCLI)
			}
			if werr != nil && (!errors.Is(werr, store.ErrWrongLogin) || !strings.Contains(werr.Error(), tt.wantWorkerMentioning)) {
				t.Fatalf("CheckWorker = %v, want ErrWrongLogin mentioning %q", werr, tt.wantWorkerMentioning)
			}
			if strings.Contains(fmt.Sprint(werr, cerr), "canary_") {
				t.Fatal("a refusal carries the password")
			}
		})
	}
}

// TestAttackAppLoginCannotLiftHalt: the worker's login, which can read the
// kill switch, cannot record a halt or a resume, through the store or by
// writing the row by hand.
func TestAttackAppLoginCannotLiftHalt(t *testing.T) {
	ctx := requireDB(t)
	conn, _ := asLogin(t, ctx, store.RoleApp)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.CurrentHalt(ctx, tx); err != nil {
		t.Fatalf("the app login cannot read the kill switch: %v", err)
	}
	attempts := []struct {
		name string
		do   func(pgx.Tx) error
	}{
		{"SetHalt", func(sp pgx.Tx) error { _, err := store.SetHalt(ctx, sp, "forged", store.ActorCLI); return err }},
		{"raw clear", func(sp pgx.Tx) error {
			_, err := sp.Exec(ctx, `INSERT INTO halt_events (kind, reason, actor) VALUES ('clear', 'forged', 'cli')`)
			return err
		}},
	}
	for _, a := range attempts {
		t.Run(a.name, func(t *testing.T) {
			sp, err := tx.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sp.Rollback(ctx) }()
			var pgErr *pgconn.PgError
			if err := a.do(sp); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("%s as the app login = %v, want permission denied", a.name, err)
			}
		})
	}
}

// TestAttackForgedDBUserOverwritten: an insert that names another login
// in db_user is recorded under the login that made it.
func TestAttackForgedDBUserOverwritten(t *testing.T) {
	ctx := requireDB(t)
	conn, name := asLogin(t, ctx, store.RoleOperator)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	forged := ownerLogin(t)
	var halt, command string
	if err := tx.QueryRow(ctx, `INSERT INTO halt_events (kind, reason, actor, db_user)
		VALUES ('set', 'forged', 'cli', $1) RETURNING db_user`, forged).Scan(&halt); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO commands
		(comment_id, delivery_id, workflow_id, repo_github_id, issue_number, command, actor, outcome, db_user)
		VALUES (424242, 'd', 'feature/1/1', 1, 1, 'status', 'github:1', 'accepted', $1) RETURNING db_user`, forged).Scan(&command); err != nil {
		t.Fatal(err)
	}
	if halt != name || command != name {
		t.Fatalf("db_user recorded as %q and %q, want the inserting login %q, not the forged %q", halt, command, name, forged)
	}
}

// TestOperatorHaltRecordsLogin: an operator login sets and clears a halt,
// and the state names the login that recorded it.
func TestOperatorHaltRecordsLogin(t *testing.T) {
	ctx := requireDB(t)
	conn, name := asLogin(t, ctx, store.RoleOperator)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	set, err := store.SetHalt(ctx, tx, "drill", store.ActorCLI)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := store.ClearHalt(ctx, tx, "drill over", store.ActorCLI)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Halted || cleared.Halted || set.DBUser != name || cleared.DBUser != name {
		t.Fatalf("set %+v, cleared %+v; want each recorded by %s", set, cleared, name)
	}
}
