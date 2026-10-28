package activity

import (
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// runAttempt runs RunAttempt in Temporal's activity test environment,
// which supplies the activity info and logger it reads.
func (f *fixture) runAttempt(in RunAttemptInput) (AttemptOutcome, error) {
	f.t.Helper()
	env := new(testsuite.WorkflowTestSuite).NewTestActivityEnvironment()
	env.RegisterActivity(f.a)
	v, err := env.ExecuteActivity(f.a.RunAttempt, in)
	if err != nil {
		return AttemptOutcome{}, err
	}
	var out AttemptOutcome
	f.must(v.Get(&out))
	return out, nil
}

func (f *fixture) runInput(task TaskRef, ref AttemptRef, ap ApproveResult) RunAttemptInput {
	return RunAttemptInput{AttemptID: ref.AttemptID, TaskID: task.TaskID, BaseSHA: ap.BaseSHA, Repo: f.repoRef(), Role: contracts.RoleImplementer}
}

// TestRunAttemptTwice runs an attempt whose model call fails, then runs
// the same attempt again: the second run returns the recorded outcome and
// creates no sandbox, so a retry can never double-run an agent.
func TestRunAttemptTwice(t *testing.T) {
	f := newFixture(t) // no turns: the first model call fails
	task, ref, ap := f.attempt()
	in := f.runInput(task, ref, ap)
	first, err := f.runAttempt(in)
	f.must(err)
	if first.State != contracts.AttemptErrored || first.Reason != contracts.ReasonProviderError {
		t.Fatalf("first run = %+v, want errored/provider_error", first)
	}
	second, err := f.runAttempt(in)
	f.must(err)
	if second != first {
		t.Errorf("second run = %+v, want the recorded %+v", second, first)
	}
	equalInt(t, "sandboxes created", f.sandboxes.creates, 1)
	equalInt(t, "live sandboxes", f.sandboxes.liveIDs(), 0)
	equalInt(t, "attempt audit rows", f.transitions(ref.AttemptID), 1)
	row, err := readAttempt(f.ctx, appPool, ref.AttemptID)
	f.must(err)
	if row.SandboxID == "" || !row.Released {
		t.Errorf("attempt row = %+v, want its sandbox recorded and released", row)
	}
}

// TestRunAttemptSandboxIDNotRecorded has the write-ahead record of the
// sandbox ID fail after Create: the sandbox is released at once, so no
// container outlives an attempt that cannot name it.
func TestRunAttemptSandboxIDNotRecorded(t *testing.T) {
	f := newFixture(t)
	task, ref, ap := f.attempt()
	// A different ID already on the row makes SetAttemptSandbox refuse.
	f.must(store.SetAttemptSandbox(f.ctx, appPool, ref.AttemptID, "someone-else"))
	_, err := f.runAttempt(f.runInput(task, ref, ap))
	requirePermanent(t, err)
	equalInt(t, "sandboxes created", f.sandboxes.creates, 1)
	equalInt(t, "live sandboxes", f.sandboxes.liveIDs(), 0)
	row, err := readAttempt(f.ctx, appPool, ref.AttemptID)
	f.must(err)
	if row.State != contracts.AttemptErrored || row.Reason != contracts.ReasonSandboxError {
		t.Errorf("attempt = %s/%s, want errored/sandbox_error", row.State, row.Reason)
	}
	if row.Released {
		t.Error("the row's sandbox was marked released, but the provider never released it")
	}
}

// TestRunAttemptRefusesWrongBase refuses to run against a commit other
// than the one the plan pinned, before any sandbox exists (FR-S19).
func TestRunAttemptRefusesWrongBase(t *testing.T) {
	f := newFixture(t)
	task, ref, ap := f.attempt()
	in := f.runInput(task, ref, ap)
	in.BaseSHA = f.gh.advance(t)
	_, err := f.runAttempt(in)
	requirePermanent(t, err)
	equalInt(t, "sandboxes created", f.sandboxes.creates, 0)
}

// TestRunAttemptRecordsToolOutputOutsideHistory runs an attempt whose
// model reads a file: the output reaches the blob store, and the
// activity's result carries only the outcome.
func TestRunAttemptRecordsToolOutputOutsideHistory(t *testing.T) {
	f := newFixture(t, ReadFileTurn("NOTES.md"))
	f.sandboxes.files["NOTES.md"] = CanaryToolOutput
	task, ref, ap := f.attempt()
	out, err := f.runAttempt(f.runInput(task, ref, ap))
	f.must(err)
	if out.State != contracts.AttemptErrored {
		t.Fatalf("outcome = %+v, want errored once the script ran out", out)
	}
	equalInt(t, "tool calls", f.count(`SELECT count(*) FROM tool_calls WHERE attempt_id = $1`, ref.AttemptID), 1)
	if !BlobsContain(t, f.blobDir, CanaryToolOutput) {
		t.Fatal("the canary never reached the blob store, so the history test proves nothing")
	}
}

// TestSweepOrphans is the startup sweep: a recorded, unreleased sandbox
// past the cutoff is released and marked; a sandbox whose ID never
// reached Postgres (the worker died between Create and
// SetAttemptSandbox) is reaped by age; a second sweep finds nothing.
func TestSweepOrphans(t *testing.T) {
	f := newFixture(t)
	_, ref, _ := f.attempt()
	recorded := f.sandboxes.orphan()
	f.must(store.SetAttemptSandbox(f.ctx, appPool, ref.AttemptID, recorded))
	f.sandboxes.orphan() // never recorded
	time.Sleep(10 * time.Millisecond)
	log := discardLogger()
	first, err := SweepOrphans(f.ctx, appPool, f.sandboxes, time.Millisecond, log)
	f.must(err)
	if first.Released < 1 || first.Reaped != 1 {
		t.Errorf("first sweep = %+v, want this attempt's sandbox released and one reaped", first)
	}
	equalInt(t, "live sandboxes", f.sandboxes.liveIDs(), 0)
	equalInt(t, "this attempt unreleased", f.count(`SELECT count(*) FROM attempts WHERE id = $1 AND sandbox_released_at IS NULL`, ref.AttemptID), 0)
	second, err := SweepOrphans(f.ctx, appPool, f.sandboxes, time.Millisecond, log)
	f.must(err)
	if second.Released != 0 || second.Reaped != 0 {
		t.Errorf("second sweep = %+v, want nothing left", second)
	}
}

// TestSweepOrphansSparesLiveAttempts leaves an attempt younger than the
// cutoff alone: its worker may still be running it.
func TestSweepOrphansSparesLiveAttempts(t *testing.T) {
	f := newFixture(t)
	_, ref, _ := f.attempt()
	f.must(store.SetAttemptSandbox(f.ctx, appPool, ref.AttemptID, "fake-live"))
	_, err := SweepOrphans(f.ctx, appPool, newFakeSandboxes(), time.Hour, discardLogger())
	f.must(err)
	equalInt(t, "this attempt unreleased", f.count(`SELECT count(*) FROM attempts WHERE id = $1 AND sandbox_released_at IS NULL`, ref.AttemptID), 1)
}
