package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/store"
)

// The same image as deploy/docker-compose.yml.
const postgresImage = "pgvector/pgvector:0.8.6-pg16"

func TestCheckSweepAfter(t *testing.T) {
	const longest = 45 * time.Minute
	tests := []struct {
		name       string
		sweepAfter time.Duration
		wantErr    bool
	}{
		{"default two hours", sweepAfter, false},
		{"just above the longest attempt", longest + time.Second, false},
		{"equal to the longest attempt", longest, true},
		{"below the longest attempt", 30 * time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSweepAfter(tt.sweepAfter, longest)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkSweepAfter(%s, %s) = %v, want error %t", tt.sweepAfter, longest, err, tt.wantErr)
			}
		})
	}
}

// TestRolesAreMilestoneOne: the worker runs the implementer, and only
// through the role map.
func TestRolesAreMilestoneOne(t *testing.T) {
	r := roles()
	if len(r) != 1 || r[contracts.RoleImplementer].PromptVersion != agent.Implementer.PromptVersion {
		t.Fatalf("roles() = %v, want the implementer alone", r)
	}
}

// secretValues are canary values for the worker's secret variables.
var secretValues = map[string]string{
	databaseURLEnv:       "canary_database_url",
	anthropicKeyEnv:      "canary_anthropic_key",
	github.AppKeyFileEnv: "canary_app_key_file",
}

func setSecrets(t *testing.T) {
	t.Helper()
	for k, v := range secretValues {
		t.Setenv(k, v)
	}
}

// TestReadSecretsUnsetsThem reads each secret and unsets it, so nothing
// the worker starts later inherits it.
func TestReadSecretsUnsetsThem(t *testing.T) {
	setSecrets(t)
	s, err := readSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if s.databaseURL != secretValues[databaseURLEnv] || s.anthropicKey != secretValues[anthropicKeyEnv] || s.appKeyFile != secretValues[github.AppKeyFileEnv] {
		t.Fatal("readSecrets did not return the values it read")
	}
	for k := range secretValues {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s is still set after readSecrets", k)
		}
	}
}

// TestReadSecretsNamesMissing names a missing secret, and no secret's
// value appears in the error.
func TestReadSecretsNamesMissing(t *testing.T) {
	setSecrets(t)
	t.Setenv(anthropicKeyEnv, "")
	_, err := readSecrets()
	if err == nil || !strings.Contains(err.Error(), anthropicKeyEnv) {
		t.Fatalf("error = %v, want it to name %s", err, anthropicKeyEnv)
	}
	for _, v := range secretValues {
		if strings.Contains(err.Error(), v) {
			t.Fatal("the error carries a secret's value")
		}
	}
}

// TestSeedRepos makes the repos table match configuration, which is the
// authority for tier and default branch (FR-SEC12): a second start with a
// changed tier updates the row rather than keeping the stored one.
func TestSeedRepos(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker for Postgres; run without -short")
	}
	ctx := context.Background()
	pool := startPostgres(ctx, t)
	const name = "austmsk/election-tally"
	d := deploy{
		Access:    map[string]llm.RepoAccess{name: {Tier: contracts.TierPublic}},
		Sandboxes: map[string]sandbox.RepoSandbox{name: {DefaultBranch: "trunk"}},
	}
	if err := seedRepos(ctx, pool, d); err != nil {
		t.Fatal(err)
	}
	checkRepo(ctx, t, pool, name, contracts.TierPublic, "trunk")
	d.Access[name] = llm.RepoAccess{Tier: contracts.TierPrivate}
	if err := seedRepos(ctx, pool, d); err != nil {
		t.Fatal(err)
	}
	checkRepo(ctx, t, pool, name, contracts.TierPrivate, "trunk")
}

func checkRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string, tier contracts.SensitivityTier, branch string) {
	t.Helper()
	var gotTier, gotBranch string
	var n int
	if err := pool.QueryRow(ctx, `SELECT tier, default_branch, count(*) OVER () FROM repos WHERE full_name = $1`, name).Scan(&gotTier, &gotBranch, &n); err != nil {
		t.Fatal(err)
	}
	if gotTier != string(tier) || gotBranch != branch || n != 1 {
		t.Fatalf("repos row = %s/%s (%d rows), want %s/%s once", gotTier, gotBranch, n, tier, branch)
	}
}

// startPostgres starts a migrated Postgres for one test and returns a
// pool connected as conclave_app, with exactly the grants a worker has.
func startPostgres(ctx context.Context, t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("conclave"), postgres.WithUsername("conclave"),
		postgres.WithPassword("canary_conclave_test_only"), postgres.BasicWaitStrategies(),
		testcontainers.WithLogger(log.New(os.Stderr, "", 0)),
	)
	if err != nil {
		t.Fatalf("starting postgres: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	if err := store.Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE conclave_app`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
