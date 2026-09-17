package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/austmsk/conclave/internal/store"
)

var wantTables = []string{
	"attempts", "cost_events", "decisions", "feature_requests", "gate_results",
	"goose_db_version", "plans", "repos", "state_transitions", "tasks",
}

func publicTables(t *testing.T, ctx context.Context) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY 1`)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scanning table name: %v", err)
		}
		names = append(names, n)
	}
	return names
}

// TestMigrationsApplyAndRollBack runs the full down path and then the full up
// path again, so a down migration that forgets a table or an up migration
// that is not repeatable fails here rather than in an emergency.
func TestMigrationsApplyAndRollBack(t *testing.T) {
	ctx, _ := testTx(t)

	if got := publicTables(t, ctx); !slices.Equal(got, wantTables) {
		t.Fatalf("after up: tables = %v, want %v", got, wantTables)
	}

	if err := store.Rollback(ctx, sqlDB); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := publicTables(t, ctx); !slices.Equal(got, []string{"goose_db_version"}) {
		t.Fatalf("after down: tables = %v, want only goose_db_version", got)
	}
	var roleExists bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'conclave_app')`).Scan(&roleExists)
	if err != nil {
		t.Fatalf("checking role: %v", err)
	}
	if roleExists {
		t.Error("after down: conclave_app role still exists")
	}

	if err := store.Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if got := publicTables(t, ctx); !slices.Equal(got, wantTables) {
		t.Fatalf("after second up: tables = %v, want %v", got, wantTables)
	}
	if err := store.Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("migrate on up-to-date schema: %v", err)
	}
}
