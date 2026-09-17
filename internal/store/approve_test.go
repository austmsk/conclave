package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

type planRow struct {
	state, baseSHA string
	approved       bool
	revision       int
}

func readPlan(t *testing.T, ctx context.Context, tx pgx.Tx, id string) planRow {
	t.Helper()
	var p planRow
	err := tx.QueryRow(ctx, `SELECT state, coalesce(base_sha, ''), approved_at IS NOT NULL, revision
		FROM plans WHERE id = $1`, id).Scan(&p.state, &p.baseSHA, &p.approved, &p.revision)
	if err != nil {
		t.Fatalf("reading plan %s: %v", id, err)
	}
	return p
}

func TestApprovePlan(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	if err := store.ApprovePlan(ctx, tx, s.plan, 0, "deadbeef", "owner:12345"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	want := planRow{state: "approved", baseSHA: "deadbeef", approved: true, revision: 1}
	if got := readPlan(t, ctx, tx, s.plan); got != want {
		t.Errorf("plan = %+v, want %+v", got, want)
	}
	if n := len(audits(t, ctx, tx, s.plan)); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}

	// A retry must not re-pin: the SHA from the first approval stands.
	err := store.ApprovePlan(ctx, tx, s.plan, 0, "cafebabe", "owner:12345")
	if !errors.Is(err, store.ErrAlreadyApplied) {
		t.Fatalf("retry: got %v, want ErrAlreadyApplied", err)
	}
	if got := readPlan(t, ctx, tx, s.plan); got != want {
		t.Errorf("after retry plan = %+v, want unchanged %+v", got, want)
	}
}

func TestApprovePlanRefusals(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	rejected, err := insertInState(ctx, tx, s, contracts.EntityPlan, "rejected")
	if err != nil {
		t.Fatalf("seeding rejected plan: %v", err)
	}
	second, err := insertInState(ctx, tx, s, contracts.EntityPlan, "proposed")
	if err != nil {
		t.Fatalf("seeding second proposed plan: %v", err)
	}
	if err := store.ApprovePlan(ctx, tx, s.plan, 0, "deadbeef", "owner:12345"); err != nil {
		t.Fatalf("approving first plan: %v", err)
	}

	var wrong *store.WrongStateError
	cases := []struct {
		name string
		plan string
		sha  string
		want func(error) bool
	}{
		{"empty base sha", second, "", func(err error) bool { return errors.Is(err, store.ErrApprovalNeedsBaseSHA) }},
		{"rejected plan", rejected, "deadbeef",
			func(err error) bool { return errors.As(err, &wrong) && wrong.Actual == "rejected" }},
		{"another version already approved", second, "deadbeef",
			func(err error) bool { return errors.Is(err, store.ErrAnotherPlanApproved) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := savepoint(t, ctx, tx)
			if err := store.ApprovePlan(ctx, sp, tc.plan, 0, tc.sha, "owner:12345"); !tc.want(err) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if got := readPlan(t, ctx, tx, second); got.state != "proposed" || got.baseSHA != "" {
		t.Errorf("second plan = %+v, want untouched proposed", got)
	}
}
