package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
)

const (
	uniqueViolation  = "23505"
	checkViolation   = "23514"
	permissionDenied = "42501"
)

func TestOneApprovedPlanPerFeatureRequest(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	var other string
	err := tx.QueryRow(ctx, `INSERT INTO feature_requests (repo_id, issue_number, title, state, workflow_id)
		VALUES ($1, 8, 'Other', 'planning', 'feature-8') RETURNING id::text`, s.repo).Scan(&other)
	if err != nil {
		t.Fatalf("seeding second feature request: %v", err)
	}

	cases := []struct {
		name      string
		feature   string
		version   int
		state     string
		wantState string
	}{
		{"first approved plan", s.feature, 2, "approved", ""},
		{"second approved plan for same feature", s.feature, 3, "approved", uniqueViolation},
		{"superseded plan alongside approved", s.feature, 4, "superseded", ""},
		{"proposed plan alongside approved", s.feature, 5, "proposed", ""},
		{"approved plan for a different feature", other, 1, "approved", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := insertPlan(ctx, tx, tc.feature, tc.version, tc.state)
			if got := sqlState(err); got != tc.wantState {
				t.Fatalf("got error %v (SQLSTATE %q), want SQLSTATE %q", err, got, tc.wantState)
			}
		})
	}
}

// insertPlan uses a savepoint so a failed insert does not abort the test's
// transaction.
func insertPlan(ctx context.Context, tx pgx.Tx, feature string, version int, state string) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	_, err = sp.Exec(ctx, `INSERT INTO plans (feature_request_id, version, state, base_sha, approved_at, body)
		VALUES ($1, $2, $3, 'abc123', now(), '{}')`, feature, version, state)
	if err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}

// TestStateChecksMatchContracts proves the database accepts exactly the
// states the transition table knows about, so the two cannot drift apart
// without a test failing.
func TestStateChecksMatchContracts(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	kinds := []struct {
		kind   contracts.EntityKind
		update string
		id     string
	}{
		{contracts.EntityFeatureRequest, `UPDATE feature_requests SET state = $1 WHERE id = $2`, s.feature},
		{contracts.EntityPlan, `UPDATE plans SET state = $1, base_sha = 'x', approved_at = now() WHERE id = $2`, s.plan},
		{contracts.EntityTask, `UPDATE tasks SET state = $1 WHERE id = $2`, s.task},
		{contracts.EntityAttempt, `UPDATE attempts SET state = $1 WHERE id = $2`, s.attempt},
	}
	for _, k := range kinds {
		states := append(contracts.States(k.kind), "no_such_state")
		for _, state := range states {
			t.Run(fmt.Sprintf("%s/%s", k.kind, state), func(t *testing.T) {
				want := ""
				if state == "no_such_state" {
					want = checkViolation
				}
				err := execSavepoint(ctx, tx, k.update, state, k.id)
				if got := sqlState(err); got != want {
					t.Fatalf("got error %v (SQLSTATE %q), want SQLSTATE %q", err, got, want)
				}
			})
		}
	}
}

func execSavepoint(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if _, err := sp.Exec(ctx, sql, args...); err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}
