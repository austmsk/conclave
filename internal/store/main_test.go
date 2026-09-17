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

	"github.com/austmsk/conclave/internal/store"
)

// The same image as deploy/docker-compose.yml, so tests see the Postgres the
// platform runs on.
const postgresImage = "pgvector/pgvector:0.8.6-pg16"

var (
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

// testTx returns a transaction that is rolled back when the test ends, so
// each test sees a clean schema and leaves nothing behind.
func testTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	if pool == nil {
		t.Skip("no database: run without -short to start one")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	return ctx, tx
}

// ids are the seed rows every test builds on: one of each kind, all in
// their initial state.
type ids struct {
	repo, feature, plan, task, attempt string
}

func seed(t *testing.T, ctx context.Context, tx pgx.Tx) ids {
	t.Helper()
	var s ids
	steps := []struct {
		dst  *string
		sql  string
		args func() []any
	}{
		{&s.repo, `INSERT INTO repos (full_name, tier, default_branch)
			VALUES ('austmsk/election-tally', 'public', 'main') RETURNING id::text`,
			func() []any { return nil }},
		{&s.feature, `INSERT INTO feature_requests (repo_id, issue_number, title, state, workflow_id)
			VALUES ($1, 7, 'Add tally export', 'planning', 'feature-7') RETURNING id::text`,
			func() []any { return []any{s.repo} }},
		{&s.plan, `INSERT INTO plans (feature_request_id, version, state, body)
			VALUES ($1, 1, 'proposed', '{}') RETURNING id::text`,
			func() []any { return []any{s.feature} }},
		{&s.task, `INSERT INTO tasks (plan_id, key, spec, state)
			VALUES ($1, 'T1', '{}', 'running') RETURNING id::text`,
			func() []any { return []any{s.plan} }},
		{&s.attempt, `INSERT INTO attempts (task_id, ordinal, state, role, prompt_version, model, provider)
			VALUES ($1, 1, 'running', 'implementer', 'v1', 'claude-fable-5-1', 'anthropic') RETURNING id::text`,
			func() []any { return []any{s.task} }},
	}
	for _, st := range steps {
		if err := tx.QueryRow(ctx, st.sql, st.args()...).Scan(st.dst); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	return s
}

// sqlState returns the SQLSTATE of a Postgres error, or "" for anything else.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
