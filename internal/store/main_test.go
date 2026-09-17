package store_test

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// The same image as deploy/docker-compose.yml, so tests see the Postgres the
// platform runs on.
const postgresImage = "pgvector/pgvector:0.8.6-pg16"

var (
	// pool connects as the database owner. Only the migration test and
	// bookkeeping (seeding committed rows, polling pg_stat_activity) use it
	// directly; everything under test runs through beginAppTx.
	pool  *pgxpool.Pool
	sqlDB *sql.DB
)

// TestMain starts one Postgres container for the package. Without Docker the
// suite fails rather than silently passing; pass -short to skip it.
func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}

	ctx := context.Background()
	ctr, dsn, err := startPostgres(ctx)
	if err != nil {
		log.Fatalf("starting postgres: %v", err)
	}

	code, err := run(ctx, m, dsn)
	if terr := ctr.Terminate(ctx); terr != nil {
		log.Printf("terminating postgres: %v", terr)
	}
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(code)
}

func startPostgres(ctx context.Context) (testcontainers.Container, string, error) {
	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("conclave"),
		postgres.WithUsername("conclave"),
		postgres.WithPassword("canary_conclave_test_only"),
		postgres.BasicWaitStrategies(),
		testcontainers.WithLogger(log.New(os.Stderr, "", 0)),
	)
	if err != nil {
		return nil, "", fmt.Errorf("running container: %w", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return ctr, "", fmt.Errorf("reading connection string: %w", err)
	}
	return ctr, dsn, nil
}

func run(ctx context.Context, m *testing.M, dsn string) (int, error) {
	var err error
	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		return 1, fmt.Errorf("opening pool: %w", err)
	}
	defer pool.Close()

	sqlDB, err = sql.Open("pgx", dsn)
	if err != nil {
		return 1, fmt.Errorf("opening database/sql: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if err := store.Migrate(ctx, sqlDB); err != nil {
		return 1, fmt.Errorf("initial migrate: %w", err)
	}
	return m.Run(), nil
}

func requireDB(t *testing.T) context.Context {
	t.Helper()
	if pool == nil {
		t.Skip("no database: run without -short to start one")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// beginAppTx opens a transaction running as the application role, so every
// statement under test is checked against the grants a worker would have.
// SET LOCAL ends with the transaction, so the pooled connection goes back
// as the owner.
func beginAppTx(ctx context.Context) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE conclave_app`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("set role: %w", err)
	}
	return tx, nil
}

// testTx returns an application-role transaction that is rolled back when
// the test ends, so each test sees a clean schema and leaves nothing behind.
func testTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	ctx := requireDB(t)
	tx, err := beginAppTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	return ctx, tx
}

// savepoint nests a transaction inside tx and rolls it back when the
// (sub)test ends, so a failing statement does not poison the outer one.
func savepoint(t *testing.T, ctx context.Context, tx pgx.Tx) pgx.Tx {
	t.Helper()
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	t.Cleanup(func() { _ = sp.Rollback(ctx) })
	return sp
}

// ids are the seed rows every test builds on: one of each kind, all in
// their initial state.
type ids struct {
	repo, feature, plan, task, attempt string
}

func seed(t *testing.T, ctx context.Context, tx pgx.Tx) ids {
	t.Helper()
	var s ids
	err := tx.QueryRow(ctx, `INSERT INTO repos (full_name, tier, default_branch)
		VALUES ('austmsk/election-tally', 'public', 'main') RETURNING id::text`).Scan(&s.repo)
	if err != nil {
		t.Fatalf("seeding repo: %v", err)
	}
	steps := []struct {
		dst   *string
		kind  contracts.EntityKind
		state string
	}{
		{&s.feature, contracts.EntityFeatureRequest, "planning"},
		{&s.plan, contracts.EntityPlan, "proposed"},
		{&s.task, contracts.EntityTask, "running"},
		{&s.attempt, contracts.EntityAttempt, "running"},
	}
	for _, st := range steps {
		id, err := insertInState(ctx, tx, s, st.kind, st.state)
		if err != nil {
			t.Fatalf("seeding %s: %v", st.kind, err)
		}
		*st.dst = id
	}
	return s
}

// insertInState inserts one record of the given kind in the given state,
// hung off the seed rows, and returns its id. A kind with no case here is an
// error, so adding a kind to contracts without teaching the tests to seed it
// fails loudly instead of being silently skipped.
func insertInState(ctx context.Context, tx pgx.Tx, s ids, kind contracts.EntityKind, state string) (string, error) {
	var query string
	var parent string
	switch kind {
	case contracts.EntityFeatureRequest:
		parent = s.repo
		query = `INSERT INTO feature_requests (repo_id, issue_number, title, state, workflow_id)
			VALUES ($1, (SELECT coalesce(max(issue_number), 0) + 1 FROM feature_requests), 'Add tally export', $2, 'feature')
			RETURNING id::text`
	case contracts.EntityPlan:
		// An approved plan must carry a pinned base commit.
		parent = s.feature
		query = `INSERT INTO plans (feature_request_id, version, state, base_sha, approved_at, body)
			VALUES ($1, (SELECT coalesce(max(version), 0) + 1 FROM plans WHERE feature_request_id = $1), $2,
			        CASE WHEN $2 = 'approved' THEN 'abc123' END, CASE WHEN $2 = 'approved' THEN now() END, '{}')
			RETURNING id::text`
	case contracts.EntityTask:
		parent = s.plan
		query = `INSERT INTO tasks (plan_id, key, spec, state)
			VALUES ($1, (SELECT 'T' || count(*) + 1 FROM tasks WHERE plan_id = $1), '{}', $2)
			RETURNING id::text`
	case contracts.EntityAttempt:
		parent = s.task
		query = `INSERT INTO attempts (task_id, ordinal, state, role, prompt_version, model, provider)
			VALUES ($1, (SELECT coalesce(max(ordinal), 0) + 1 FROM attempts WHERE task_id = $1), $2,
			        'implementer', 'v1', 'claude-fable-5-1', 'anthropic')
			RETURNING id::text`
	default:
		return "", fmt.Errorf("no seed for entity kind %q", kind)
	}
	var id string
	if err := tx.QueryRow(ctx, query, parent, state).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// sqlState returns the SQLSTATE of a Postgres error, or "" for anything else.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
