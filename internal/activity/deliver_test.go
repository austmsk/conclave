package activity

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/testsuite"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/store"
)

const deliverDiff = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n"

// passedAttempt takes a fresh issue to an attempt that passed: its
// artifact frozen in the blob store, a passing result for every required
// gate but skip (none when empty), and the attempt finished passed. It
// returns the attempt, its artifact hash and the pinned base.
func (f *fixture) passedAttempt(skip contracts.GateName) (attemptID, hash, base string) {
	f.t.Helper()
	_, ref, ap := f.attempt()
	diff := []byte(deliverDiff)
	_, err := f.a.d.Blobs.Put(f.ctx, repoID, gate.Envelope(ap.BaseSHA, diff))
	f.must(err)
	hash = gate.HashArtifact(ap.BaseSHA, diff)
	f.must(store.SetArtifactHash(f.ctx, appPool, ref.AttemptID, hash))
	rows := make([]contracts.GateResult, 0, len(gate.RequiredGates))
	for _, g := range gate.RequiredGates {
		if g == skip {
			continue
		}
		rows = append(rows, contracts.GateResult{
			AttemptID: ref.AttemptID, Gate: g, Passed: true, ArtifactHash: hash,
			Mode: contracts.GatingCleanRoom, RanAt: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
		})
	}
	_, err = store.NewGateResults(appPool).Record(f.ctx, rows)
	f.must(err)
	row, err := readAttempt(f.ctx, appPool, ref.AttemptID)
	f.must(err)
	f.must(pgx.BeginFunc(f.ctx, appPool, func(tx pgx.Tx) error {
		return store.FinishAttempt(f.ctx, tx, store.FinishParams{
			AttemptID: ref.AttemptID, Revision: row.Revision, To: contracts.AttemptPassed, Actor: "workflow:test",
		})
	}))
	return ref.AttemptID, hash, ap.BaseSHA
}

// TestDeliverVerifiesPassedArtifact runs the deliver stub twice on a
// passed, verified artifact: both runs report the same stub result,
// naming the hash and base it would push, and neither writes anything or
// calls GitHub.
func TestDeliverVerifiesPassedArtifact(t *testing.T) {
	f := newFixture(t)
	attemptID, hash, base := f.passedAttempt("")
	audit, clones := f.transitions(attemptID), f.gh.clones
	env := new(testsuite.WorkflowTestSuite).NewTestActivityEnvironment()
	env.RegisterActivity(f.a)
	want := DeliverResult{ArtifactHash: hash, BaseSHA: base, Detail: DetailDeliveryStub}
	for range 2 {
		v, err := env.ExecuteActivity(f.a.Deliver, attemptID, hash, base)
		f.must(err)
		var got DeliverResult
		f.must(v.Get(&got))
		if got != want {
			t.Fatalf("Deliver = %+v, want %+v", got, want)
		}
	}
	equalInt(t, "attempt audit rows", f.transitions(attemptID), audit)
	equalInt(t, "GitHub clones", f.gh.clones, clones)
	equalInt(t, "GitHub comments", f.gh.creates, 0)
}

// TestDeliverRefusesUnverified refuses a passed attempt when what it is
// asked to push is not what was verified: another hash, another base, or
// an artifact missing a required gate result.
func TestDeliverRefusesUnverified(t *testing.T) {
	keep := func(hash, base string) (string, string) { return hash, base }
	cases := []struct {
		name string
		skip contracts.GateName
		ask  func(hash, base string) (string, string)
	}{
		{"another hash", "", func(_, base string) (string, string) { return "sha256:" + base, base }},
		{"another base", "", func(hash, _ string) (string, string) { return hash, "fedcba9876543210fedcba9876543210fedcba98" }},
		{"a gate result missing", contracts.GateLint, keep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			attemptID, hash, base := f.passedAttempt(tc.skip)
			hash, base = tc.ask(hash, base)
			env := new(testsuite.WorkflowTestSuite).NewTestActivityEnvironment()
			env.RegisterActivity(f.a)
			_, err := env.ExecuteActivity(f.a.Deliver, attemptID, hash, base)
			if err == nil {
				t.Fatal("Deliver accepted an artifact it should not push")
			}
		})
	}
}
