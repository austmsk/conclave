package store_test

import (
	"fmt"
	"testing"

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

	other, err := insertInState(ctx, tx, s, contracts.EntityFeatureRequest, "planning")
	if err != nil {
		t.Fatalf("seeding second feature request: %v", err)
	}

	cases := []struct {
		name    string
		feature string
		state   string
		want    string
	}{
		{"first approved plan", s.feature, "approved", ""},
		{"second approved plan for same feature", s.feature, "approved", uniqueViolation},
		{"superseded plan alongside approved", s.feature, "superseded", ""},
		{"proposed plan alongside approved", s.feature, "proposed", ""},
		{"approved plan for a different feature", other, "approved", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each insert is its own savepoint so a rejected one does not
			// abort the shared transaction, but accepted ones stay visible
			// to the following cases.
			sp, err := tx.Begin(ctx)
			if err != nil {
				t.Fatalf("savepoint: %v", err)
			}
			_, err = insertInState(ctx, sp, ids{feature: tc.feature}, contracts.EntityPlan, tc.state)
			if got := sqlState(err); got != tc.want {
				t.Fatalf("got error %v (SQLSTATE %q), want SQLSTATE %q", err, got, tc.want)
			}
			if err != nil {
				_ = sp.Rollback(ctx)
				return
			}
			if err := sp.Commit(ctx); err != nil {
				t.Fatalf("release savepoint: %v", err)
			}
		})
	}
}

// TestSchemaAcceptsExactlyTheContractStates proves the CHECK constraints and
// the transition table agree: every state the table knows is storable, and a
// state it does not know is rejected.
func TestSchemaAcceptsExactlyTheContractStates(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)

	for _, kind := range contracts.EntityKinds() {
		states := append(contracts.States(kind), "no_such_state")
		for _, state := range states {
			t.Run(fmt.Sprintf("%s/%s", kind, state), func(t *testing.T) {
				want := ""
				if state == "no_such_state" {
					want = checkViolation
				}
				sp := savepoint(t, ctx, tx)
				_, err := insertInState(ctx, sp, s, kind, state)
				if got := sqlState(err); got != want {
					t.Fatalf("got error %v (SQLSTATE %q), want SQLSTATE %q", err, got, want)
				}
			})
		}
	}
}
