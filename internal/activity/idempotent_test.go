package activity

import (
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/store"
)

// Every activity runs twice here and the test asserts one effect: one
// row, one audit entry, one comment, one sandbox release (RL-2). Where an
// activity has an outside action, a second case kills it between the
// outside action and its report, and the retry closes the gap.

func TestAnalyzeTwice(t *testing.T) {
	f := newFixture(t)
	first := f.analyzed()
	second := f.analyzed()
	if first.FeatureRequestID != second.FeatureRequestID || first.PlanID != second.PlanID {
		t.Fatalf("second run returned %+v, first %+v", second, first)
	}
	equalInt(t, "feature requests", f.count(`SELECT count(*) FROM feature_requests WHERE id = $1`, first.FeatureRequestID), 1)
	equalInt(t, "plans", f.count(`SELECT count(*) FROM plans WHERE feature_request_id = $1`, first.FeatureRequestID), 1)
	equalInt(t, "audit rows", f.transitions(first.FeatureRequestID), 0)
	var body PlanBody
	_, body, err := f.a.planOf(f.ctx, first.PlanID)
	f.must(err)
	if body.ConfigDigests["repos.yaml"] == "" || body.Profile.Language == "" || len(body.Task.Criteria) != 2 {
		t.Errorf("plan body = %+v, want digests, profile and two criteria", body)
	}
}

// TestAnalyzeStrangerIssue does not plan from an issue the owner did not
// write, and does not record its body as a plan.
func TestAnalyzeStrangerIssue(t *testing.T) {
	f := newFixture(t)
	issue := f.ownerIssue()
	issue.Author = github.Author{ID: testOwner + 1, Type: "User"}
	res, err := f.a.Analyze(f.ctx, AnalyzeInput{WorkflowID: f.workflowID(), Repo: f.repoRef(), Issue: issue})
	f.must(err)
	if res.Problem == "" || res.PlanID != "" {
		t.Fatalf("result = %+v, want a problem and no plan", res)
	}
	equalInt(t, "plans", f.count(`SELECT count(*) FROM plans WHERE feature_request_id = $1`, res.FeatureRequestID), 0)
}

// TestAnalyzeOtherWorkflowRefused refuses an issue already owned by
// another workflow ID, permanently.
func TestAnalyzeOtherWorkflowRefused(t *testing.T) {
	f := newFixture(t)
	f.analyzed()
	_, err := f.a.Analyze(f.ctx, AnalyzeInput{WorkflowID: "feature/other", Repo: f.repoRef(), Issue: f.ownerIssue()})
	requirePermanent(t, err)
}

func TestPostPlanCommentTwice(t *testing.T) {
	tests := []struct {
		name        string
		lostReplies int
	}{
		{name: "both runs report"},
		{name: "the first post's reply is lost", lostReplies: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			res := f.analyzed()
			f.gh.lostReplies = tt.lostReplies
			in := PlanCommentInput{Repo: f.repoRef(), Issue: f.issue, PlanID: res.PlanID}
			_, firstErr := f.a.PostPlanComment(f.ctx, in)
			if (firstErr != nil) != (tt.lostReplies > 0) {
				t.Fatalf("first run error = %v", firstErr)
			}
			second, err := f.a.PostPlanComment(f.ctx, in)
			f.must(err)
			equalInt(t, "comments created", f.gh.creates, 1)
			if second.Created || second.CommentID != f.gh.comments[0].ID {
				t.Errorf("second run = %+v, want the existing comment %d", second, f.gh.comments[0].ID)
			}
		})
	}
}

// TestPostPlanCommentWrongIssue refuses to post a plan on another issue.
func TestPostPlanCommentWrongIssue(t *testing.T) {
	f := newFixture(t)
	res := f.analyzed()
	_, err := f.a.PostPlanComment(f.ctx, PlanCommentInput{Repo: f.repoRef(), Issue: f.issue + 1000, PlanID: res.PlanID})
	requirePermanent(t, err)
	equalInt(t, "comments created", f.gh.creates, 0)
}

func TestTransitionTwice(t *testing.T) {
	f := newFixture(t)
	res := f.analyzed()
	in := TransitionInput{
		Kind: contracts.EntityFeatureRequest, ID: res.FeatureRequestID, From: string(contracts.FeaturePlanning),
		To: string(contracts.FeatureNeedsAttention), Revision: res.Revision, Reason: "error:x", Actor: "workflow:test",
	}
	first, err := f.a.Transition(f.ctx, in)
	f.must(err)
	second, err := f.a.Transition(f.ctx, in)
	f.must(err)
	equalInt(t, "revision", second, first)
	equalInt(t, "audit rows", f.transitions(res.FeatureRequestID), 1)
	// A stale revision somebody else moved past is permanent.
	in.From, in.To = string(contracts.FeatureNeedsAttention), string(contracts.FeatureCancelled)
	_, err = f.a.Transition(f.ctx, in)
	requirePermanent(t, err)
}

// TestTransitionRefusesAttempts keeps attempts on FinishAttempt.
func TestTransitionRefusesAttempts(t *testing.T) {
	f := newFixture(t)
	_, err := f.a.Transition(f.ctx, TransitionInput{Kind: contracts.EntityAttempt, ID: "x"})
	requirePermanent(t, err)
}

// TestApproveTwice pins the base once: a retry after the default branch
// moved returns the commit pinned the first time and does not clone.
func TestApproveTwice(t *testing.T) {
	f := newFixture(t)
	res, first := f.approved()
	clones := f.gh.clones
	f.gh.advance(t)
	second, err := f.a.Approve(f.ctx, ApproveInput{
		Repo: f.repoRef(), FeatureRequestID: res.FeatureRequestID, FeatureRevision: first.FeatureRevision - 1,
		PlanID: res.PlanID, PlanRevision: first.PlanRevision - 1, Actor: store.ActorGitHub(testOwner),
	})
	f.must(err)
	equalStr(t, "pinned base", second.BaseSHA, first.BaseSHA)
	equalInt(t, "clones", f.gh.clones, clones)
	equalInt(t, "plan audit rows", f.transitions(res.PlanID), 1)
	equalInt(t, "feature request audit rows", f.transitions(res.FeatureRequestID), 2)
}

func TestCreateTaskTwice(t *testing.T) {
	f := newFixture(t)
	res, _ := f.approved()
	first, err := f.a.CreateTask(f.ctx, res.PlanID)
	f.must(err)
	second, err := f.a.CreateTask(f.ctx, res.PlanID)
	f.must(err)
	equalStr(t, "task", second.TaskID, first.TaskID)
	equalInt(t, "tasks", f.count(`SELECT count(*) FROM tasks WHERE plan_id = $1`, res.PlanID), 1)
}

// TestCreateTaskUnapproved refuses a task under a plan nobody approved.
func TestCreateTaskUnapproved(t *testing.T) {
	f := newFixture(t)
	res := f.analyzed()
	_, err := f.a.CreateTask(f.ctx, res.PlanID)
	requirePermanent(t, err)
}

func TestPrepareAttemptTwice(t *testing.T) {
	f := newFixture(t)
	task, first, _ := f.attempt()
	second, err := f.a.PrepareAttempt(f.ctx, PrepareAttemptInput{TaskID: task.TaskID, Ordinal: 1, Role: contracts.RoleImplementer})
	f.must(err)
	equalStr(t, "attempt", second.AttemptID, first.AttemptID)
	equalInt(t, "attempts", f.count(`SELECT count(*) FROM attempts WHERE task_id = $1`, task.TaskID), 1)
	if first.Timeout != 45*time.Minute {
		t.Errorf("timeout = %v, want the 45m sum", first.Timeout)
	}
}

func TestSettleAttemptTwice(t *testing.T) {
	f := newFixture(t)
	_, ref, _ := f.attempt()
	in := SettleInput{AttemptID: ref.AttemptID, Reason: contracts.ReasonSandboxError, Actor: "workflow:test"}
	first, err := f.a.SettleAttempt(f.ctx, in)
	f.must(err)
	in.Reason = contracts.ReasonCancelled
	second, err := f.a.SettleAttempt(f.ctx, in)
	f.must(err)
	if first != second || first.State != contracts.AttemptErrored || first.Reason != contracts.ReasonSandboxError {
		t.Errorf("settled %+v then %+v, want errored/sandbox_error both times", first, second)
	}
	equalInt(t, "audit rows", f.transitions(ref.AttemptID), 1)
}

func TestReleaseAttemptSandboxTwice(t *testing.T) {
	tests := []struct {
		name string
		// removedFirst has Docker remove the container before the
		// activity runs: a worker that died after Release and before
		// MarkSandboxReleased.
		removedFirst bool
	}{
		{name: "both runs"},
		{name: "died between Docker and Postgres", removedFirst: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			_, ref, _ := f.attempt()
			id := f.sandboxes.orphan()
			f.must(store.SetAttemptSandbox(f.ctx, appPool, ref.AttemptID, id))
			if tt.removedFirst {
				f.must(f.sandboxes.Release(f.ctx, id))
			}
			f.must(f.a.ReleaseAttemptSandbox(f.ctx, ref.AttemptID))
			releases := f.sandboxes.releases
			f.must(f.a.ReleaseAttemptSandbox(f.ctx, ref.AttemptID))
			equalInt(t, "releases by the second run", f.sandboxes.releases, releases)
			equalInt(t, "live sandboxes", f.sandboxes.liveIDs(), 0)
			equalInt(t, "unreleased rows", f.count(`SELECT count(*) FROM attempts WHERE id = $1 AND sandbox_released_at IS NULL`, ref.AttemptID), 0)
		})
	}
}

// TestReleaseAttemptSandboxNothingRecorded is a no-op for an attempt
// that never recorded a sandbox.
func TestReleaseAttemptSandboxNothingRecorded(t *testing.T) {
	f := newFixture(t)
	_, ref, _ := f.attempt()
	f.must(f.a.ReleaseAttemptSandbox(f.ctx, ref.AttemptID))
	equalInt(t, "releases", f.sandboxes.releases, 0)
}

func TestReleaseFeatureSandboxesTwice(t *testing.T) {
	f := newFixture(t)
	_, ref, _ := f.attempt()
	f.must(store.SetAttemptSandbox(f.ctx, appPool, ref.AttemptID, f.sandboxes.orphan()))
	frID := f.featureOf(ref.AttemptID)
	first, err := f.a.ReleaseFeatureSandboxes(f.ctx, frID)
	f.must(err)
	second, err := f.a.ReleaseFeatureSandboxes(f.ctx, frID)
	f.must(err)
	equalInt(t, "first run", first, 1)
	equalInt(t, "second run", second, 0)
	equalInt(t, "live sandboxes", f.sandboxes.liveIDs(), 0)
}

func TestCancelTaskTwice(t *testing.T) {
	f := newFixture(t)
	task, ref, _ := f.attempt()
	f.must(store.SetAttemptSandbox(f.ctx, appPool, ref.AttemptID, f.sandboxes.orphan()))
	in := CancelTaskInput{TaskID: task.TaskID, Actor: store.ActorGitHub(testOwner)}
	f.must(f.a.CancelTask(f.ctx, in))
	f.must(f.a.CancelTask(f.ctx, in))
	now, err := f.a.LoadTask(f.ctx, task.TaskID)
	f.must(err)
	equalStr(t, "task", string(now.Status), string(contracts.TaskCancelled))
	equalInt(t, "task audit rows", f.transitions(task.TaskID), 1)
	equalInt(t, "attempt audit rows", f.transitions(ref.AttemptID), 1)
	equalInt(t, "releases", f.sandboxes.releases, 1)
	row, err := readAttempt(f.ctx, appPool, ref.AttemptID)
	f.must(err)
	if row.State != contracts.AttemptErrored || row.Reason != contracts.ReasonCancelled || !row.Released {
		t.Errorf("attempt = %+v, want errored/cancelled and released", row)
	}
}

// TestDeliverRefusesUnpassed checks the deliver stub, which only reads,
// refuses an attempt that did not pass, the same way twice, and changes
// nothing.
func TestDeliverRefusesUnpassed(t *testing.T) {
	f := newFixture(t)
	_, ref, ap := f.attempt()
	env := new(testsuite.WorkflowTestSuite).NewTestActivityEnvironment()
	env.RegisterActivity(f.a)
	for range 2 {
		_, err := env.ExecuteActivity(f.a.Deliver, ref.AttemptID, "sha256:00", ap.BaseSHA)
		requirePermanent(t, err)
	}
	equalInt(t, "attempt audit rows", f.transitions(ref.AttemptID), 0)
}

// featureOf returns the feature request above an attempt.
func (f *fixture) featureOf(attemptID string) string {
	f.t.Helper()
	row, err := readAttempt(f.ctx, appPool, attemptID)
	f.must(err)
	pc, err := readPlanContext(f.ctx, appPool, row.TaskID)
	f.must(err)
	return pc.FeatureRequestID
}

// requirePermanent fails unless err is a non-retryable ApplicationError.
func requirePermanent(t *testing.T, err error) {
	t.Helper()
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || !app.NonRetryable() {
		t.Fatalf("error = %v, want a non-retryable ApplicationError", err)
	}
}

func equalInt(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", what, got, want)
	}
}

func equalStr(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}
