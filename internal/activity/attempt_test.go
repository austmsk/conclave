package activity

import (
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
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
	return RunAttemptInput{AttemptID: ref.AttemptID, TaskID: task.TaskID, BaseSHA: ap.BaseSHA, RepoFullName: testRepo, Role: contracts.RoleImplementer}
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

// TestTraceableFromWorkflowID walks the join from the workflow ID down
// to an attempt's evidence after a fake run: feature_requests.workflow_id
// to plans, tasks and attempts, then the attempt's tool calls, cost
// events and state transitions. The run stops before the gate, so
// gate_results is joined but empty; ticket 10 walks it over a gated run.
func TestTraceableFromWorkflowID(t *testing.T) {
	f := newFixture(t, ReadFileTurn("NOTES.md"))
	f.sandboxes.files["NOTES.md"] = "notes"
	task, ref, ap := f.attempt()
	_, err := f.runAttempt(f.runInput(task, ref, ap))
	f.must(err)
	const join = `FROM feature_requests fr
		JOIN plans p ON p.feature_request_id = fr.id
		JOIN tasks t ON t.plan_id = p.id
		JOIN attempts a ON a.task_id = t.id
		WHERE fr.workflow_id = $1`
	counts := []struct {
		what  string
		query string
		want  int
	}{
		{"attempts", `SELECT count(*) ` + join, 1},
		{"tool calls", `SELECT count(*) ` + join + ` AND EXISTS (SELECT 1 FROM tool_calls c WHERE c.attempt_id = a.id)`, 1},
		{"cost events", `SELECT count(*) FROM cost_events c WHERE c.attempt_id IN (SELECT a.id ` + join + `)`, 1},
		{"attempt transitions", `SELECT count(*) FROM state_transitions s WHERE s.entity_id IN (SELECT a.id ` + join + `)`, 1},
		{"feature request transitions", `SELECT count(*) FROM state_transitions s WHERE s.entity_id IN (SELECT fr.id ` + join + `)`, 2},
		{"gate results", `SELECT count(*) FROM gate_results g WHERE g.attempt_id IN (SELECT a.id ` + join + `)`, 0},
	}
	for _, c := range counts {
		equalInt(t, c.what, f.count(c.query, f.workflowID()), c.want)
	}
}

// TestSweepOrphansActivityRefusesShortCutoff: the scheduled sweep refuses
// a cutoff that does not exceed the longest sandbox hold, Finalize's
// included, so a schedule edited by hand cannot reap a live sandbox.
func TestSweepOrphansActivityRefusesShortCutoff(t *testing.T) {
	f := newFixture(t)
	_, ref, _ := f.attempt()
	f.must(store.SetAttemptSandbox(f.ctx, appPool, ref.AttemptID, f.sandboxes.orphan()))
	env := f.deliveryEnv()
	hold := f.a.d.LongestSandboxHold()
	if hold < FinalizeTimeout {
		t.Fatalf("LongestSandboxHold = %s, below Finalize's %s", hold, FinalizeTimeout)
	}
	for _, cutoff := range []time.Duration{0, time.Millisecond, hold} {
		_, err := env.ExecuteActivity(f.a.SweepOrphans, SweepInput{OlderThan: cutoff})
		var app *temporal.ApplicationError
		if !errors.As(err, &app) || !app.NonRetryable() {
			t.Fatalf("cutoff %s: got %v, want a permanent refusal", cutoff, err)
		}
	}
	equalInt(t, "live sandboxes", f.sandboxes.liveIDs(), 1)
}

// TestSweepOrphansActivityTwice runs the scheduled sweep twice with a
// cutoff past the hold: the young attempt's recorded sandbox is spared
// both times, and the second run finds nothing. (The fake provider's Reap
// ignores age, so it removes the container the first time; the database
// side is what this test checks.)
func TestSweepOrphansActivityTwice(t *testing.T) {
	f := newFixture(t)
	_, ref, _ := f.attempt()
	f.must(store.SetAttemptSandbox(f.ctx, appPool, ref.AttemptID, f.sandboxes.orphan()))
	env := f.deliveryEnv()
	var results []SweepResult
	for range 2 {
		v, err := env.ExecuteActivity(f.a.SweepOrphans, SweepInput{OlderThan: f.a.d.LongestSandboxHold() + time.Minute})
		f.must(err)
		var res SweepResult
		f.must(v.Get(&res))
		results = append(results, res)
	}
	equalInt(t, "first released", results[0].Released, 0)
	if results[1] != (SweepResult{}) {
		t.Fatalf("second sweep = %+v, want nothing left", results[1])
	}
	equalInt(t, "this attempt unreleased", f.count(`SELECT count(*) FROM attempts WHERE id = $1 AND sandbox_released_at IS NULL`, ref.AttemptID), 1)
}
