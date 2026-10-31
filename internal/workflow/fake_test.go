package workflow

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	sdkactivity "go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
)

// Fixture identity.
const (
	ownerID   = int64(4242)
	frID      = "11111111-1111-4111-8111-111111111111"
	planID    = "22222222-2222-4222-8222-222222222222"
	taskID    = "33333333-3333-4333-8333-333333333333"
	baseSHA   = "0123456789abcdef0123456789abcdef01234567"
	artifact  = "sha256:aaaa"
	issueNum  = 7
	repoID    = int64(99)
	sandboxOf = "sandbox-"
)

func attemptID(ordinal int) string {
	return fmt.Sprintf("44444444-4444-4444-8444-%012d", ordinal)
}

// world is a stateful fake of the activities: the rows they would write
// and the sandboxes they would hold, behind the same names and signatures.
// It is a fake, not a mock: tests assert on what the world holds after the
// run, not on the calls made to reach it.
type world struct {
	mu sync.Mutex

	fr       contracts.FeatureRequestStatus
	frRev    int
	frReason string
	task     contracts.TaskStatus
	taskRev  int
	attempts []fakeAttempt
	approved int
	comments int
	delivers int
	calls    map[string]int
	frMoves  []string

	// problem makes Analyze report an issue that cannot be planned.
	problem string
	// duringAnalyze, when set, runs inside Analyze before it returns.
	duringAnalyze func()
	// run is what RunAttempt does for an ordinal. It records the attempt
	// through finish and returns what the activity would.
	run func(ctx context.Context, w *world, ordinal int) (activity.AttemptOutcome, error)
	// fail, when set for an activity name, is returned by that activity.
	fail map[string]error
	// refuseMovesTo makes the next n feature request moves to a status
	// fail permanently, as a move Postgres refuses would.
	refuseMovesTo map[contracts.FeatureRequestStatus]int

	// caps is what CheckCaps reports; duringCheckCaps, when set, runs
	// inside CheckCaps before it returns.
	caps            activity.CapCheck
	duringCheckCaps func()
	// status is the one status comment on the issue, nil until posted;
	// statusPosts counts creates, statusEdits edits.
	status      *activity.RunView
	statusPosts int
	statusEdits int
	// audit is the commands table, keyed by comment ID; auditOrder is
	// insertion order.
	audit      map[int64]activity.RecordCommandInput
	auditOrder []int64
	// replies holds the one reply per command comment.
	replies map[int64]activity.ReplyInput
	// lookup is what LookupFeature reports.
	lookup activity.LookupFeatureResult

	// pr is the delivery: the pull_requests row the delivery activities
	// would write, and what they did on GitHub.
	pr fakeDelivery
	// stops queues, per delivery activity name, the stops it returns,
	// one per call; an empty queue means the step finishes. failOnce
	// returns its error from the next call of the named activity only.
	stops    map[string][]activity.DeliveryStep
	failOnce map[string]error
	// ci is how OpenPullRequest says to wait for CI; zero means a minute
	// between reads and an hour in all.
	ci activity.CIWait
	// checks answers the nth ReadChecks of the run (from 1); nil means
	// every read finds CI passed.
	checks func(n int) activity.ChecksResult
}

// fakeDelivery is the pull_requests row and GitHub's side of a delivery,
// as the idempotent delivery activities leave them: each step's effect
// happens once, however often the step runs.
type fakeDelivery struct {
	written, landed, finalized, opened, ready bool
	// modelCalls counts pr_writer calls, landings integration pushes of
	// the original commit, opens pull requests opened, readies drafts
	// marked ready, and reads ReadChecks calls.
	modelCalls, landings, opens, readies, reads int
}

type fakeAttempt struct {
	state    contracts.AttemptState
	reason   contracts.FailureReason
	sandbox  string
	released bool
}

func newWorld() *world {
	return &world{
		fr: "", calls: map[string]int{}, fail: map[string]error{}, refuseMovesTo: map[contracts.FeatureRequestStatus]int{},
		audit: map[int64]activity.RecordCommandInput{}, replies: map[int64]activity.ReplyInput{},
		stops: map[string][]activity.DeliveryStep{}, failOnce: map[string]error{},
	}
}

func (w *world) count(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls[name]++
	if err, ok := w.failOnce[name]; ok {
		delete(w.failOnce, name)
		return err
	}
	return w.fail[name]
}

func (w *world) called(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[name]
}

// finish records attempt ordinal's terminal state, as FinishAttempt would:
// once, from running.
func (w *world) finish(ordinal int, state contracts.AttemptState, reason contracts.FailureReason) activity.AttemptOutcome {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := &w.attempts[ordinal-1]
	if a.state == contracts.AttemptRunning {
		a.state, a.reason = state, reason
	}
	out := activity.AttemptOutcome{AttemptID: attemptID(ordinal), State: a.state, Reason: a.reason}
	if a.state == contracts.AttemptPassed {
		out.ArtifactHash = artifact
	}
	return out
}

// createSandbox records a sandbox on attempt ordinal, as SetAttemptSandbox
// would right after Create.
func (w *world) createSandbox(ordinal int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attempts[ordinal-1].sandbox = fmt.Sprintf("%s%d", sandboxOf, ordinal)
}

// unreleased is every recorded sandbox not yet released.
func (w *world) unreleased() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, a := range w.attempts {
		if a.sandbox != "" && !a.released {
			out = append(out, a.sandbox)
		}
	}
	return out
}

func (w *world) moves() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.frMoves)
}

func (w *world) taskStatus() contracts.TaskStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.task
}

func (w *world) attemptStates() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, a := range w.attempts {
		out = append(out, string(a.state)+"/"+string(a.reason))
	}
	return out
}

func ordinalOf(id string) int {
	var n int
	_, _ = fmt.Sscanf(id[len(id)-12:], "%d", &n)
	return n
}

func (w *world) Analyze(_ context.Context, in activity.AnalyzeInput) (activity.AnalyzeResult, error) {
	if err := w.count("Analyze"); err != nil {
		return activity.AnalyzeResult{}, err
	}
	if w.duringAnalyze != nil {
		w.duringAnalyze()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fr == "" {
		w.fr = contracts.FeaturePlanning
	}
	res := activity.AnalyzeResult{FeatureRequestID: frID, Status: w.fr, Revision: w.frRev, Problem: w.problem}
	if w.problem == "" {
		res.PlanID, res.PlanStatus = planID, contracts.PlanProposed
	}
	return res, nil
}

func (w *world) PostPlanComment(_ context.Context, in activity.PlanCommentInput) (activity.PlanCommentResult, error) {
	if err := w.count("PostPlanComment"); err != nil {
		return activity.PlanCommentResult{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.comments++
	return activity.PlanCommentResult{CommentID: 1, Created: true}, nil
}

func (w *world) Transition(_ context.Context, in activity.TransitionInput) (int, error) {
	if err := w.count("Transition"); err != nil {
		return 0, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// The store refuses a move the state table does not allow (RL-13), so
	// the fake does too: an illegal move fails the tests, not production.
	if !contracts.CanTransition(in.Kind, in.From, in.To) {
		return 0, temporal.NewNonRetryableApplicationError("illegal move "+in.From+">"+in.To, activity.ErrTypePermanent, nil)
	}
	switch in.Kind {
	case contracts.EntityFeatureRequest:
		if string(w.fr) != in.From || w.frRev != in.Revision {
			return 0, temporal.NewNonRetryableApplicationError("wrong state", activity.ErrTypePermanent, nil)
		}
		if to := contracts.FeatureRequestStatus(in.To); w.refuseMovesTo[to] > 0 {
			w.refuseMovesTo[to]--
			return 0, temporal.NewNonRetryableApplicationError("move refused", activity.ErrTypePermanent, nil)
		}
		w.fr, w.frReason = contracts.FeatureRequestStatus(in.To), in.Reason
		w.frRev++
		move := in.From + ">" + in.To
		if in.Reason != "" {
			move += " " + in.Reason
		}
		w.frMoves = append(w.frMoves, move)
		return w.frRev, nil
	default:
		if string(w.task) != in.From || w.taskRev != in.Revision {
			return 0, temporal.NewNonRetryableApplicationError("wrong state", activity.ErrTypePermanent, nil)
		}
		w.task = contracts.TaskStatus(in.To)
		w.taskRev++
		return w.taskRev, nil
	}
}

func (w *world) Approve(_ context.Context, in activity.ApproveInput) (activity.ApproveResult, error) {
	if err := w.count("Approve"); err != nil {
		return activity.ApproveResult{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fr != contracts.FeatureAwaitingApproval || !w.fr.CanMoveTo(contracts.FeatureExecuting) || in.FeatureRevision != w.frRev {
		return activity.ApproveResult{}, temporal.NewNonRetryableApplicationError("not awaiting approval", activity.ErrTypePermanent, nil)
	}
	w.approved++
	w.fr = contracts.FeatureExecuting
	w.frRev++
	w.frMoves = append(w.frMoves, "awaiting_approval>executing")
	return activity.ApproveResult{BaseSHA: baseSHA, FeatureRevision: w.frRev, PlanRevision: in.PlanRevision + 1}, nil
}

func (w *world) CreateTask(_ context.Context, plan string) (activity.TaskRef, error) {
	if err := w.count("CreateTask"); err != nil {
		return activity.TaskRef{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.task == "" {
		w.task = contracts.TaskRunning
	}
	return activity.TaskRef{TaskID: taskID, Key: "T1", Status: w.task, Revision: w.taskRev}, nil
}

func (w *world) LoadTask(_ context.Context, id string) (activity.TaskRef, error) {
	if err := w.count("LoadTask"); err != nil {
		return activity.TaskRef{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return activity.TaskRef{TaskID: id, Key: "T1", Status: w.task, Revision: w.taskRev}, nil
}

func (w *world) PrepareAttempt(_ context.Context, in activity.PrepareAttemptInput) (activity.AttemptRef, error) {
	if err := w.count("PrepareAttempt"); err != nil {
		return activity.AttemptRef{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.attempts) < in.Ordinal {
		w.attempts = append(w.attempts, fakeAttempt{state: contracts.AttemptRunning})
	}
	a := w.attempts[in.Ordinal-1]
	return activity.AttemptRef{AttemptID: attemptID(in.Ordinal), Ordinal: in.Ordinal, State: a.state, Reason: a.reason, Timeout: time.Hour}, nil
}

func (w *world) RunAttempt(ctx context.Context, in activity.RunAttemptInput) (activity.AttemptOutcome, error) {
	if err := w.count("RunAttempt"); err != nil {
		return activity.AttemptOutcome{}, err
	}
	n := ordinalOf(in.AttemptID)
	w.createSandbox(n)
	return w.run(ctx, w, n)
}

func (w *world) SettleAttempt(_ context.Context, in activity.SettleInput) (activity.AttemptOutcome, error) {
	if err := w.count("SettleAttempt"); err != nil {
		return activity.AttemptOutcome{}, err
	}
	return w.finish(ordinalOf(in.AttemptID), contracts.AttemptErrored, in.Reason), nil
}

func (w *world) ReleaseAttemptSandbox(_ context.Context, id string) error {
	if err := w.count("ReleaseAttemptSandbox"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := ordinalOf(id); n >= 1 && n <= len(w.attempts) && w.attempts[n-1].sandbox != "" {
		w.attempts[n-1].released = true
	}
	return nil
}

func (w *world) ReleaseFeatureSandboxes(_ context.Context, id string) (int, error) {
	if err := w.count("ReleaseFeatureSandboxes"); err != nil {
		return 0, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for i := range w.attempts {
		if w.attempts[i].sandbox != "" && !w.attempts[i].released {
			w.attempts[i].released = true
			n++
		}
	}
	return n, nil
}

func (w *world) CancelTask(_ context.Context, in activity.CancelTaskInput) error {
	if err := w.count("CancelTask"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.attempts {
		if w.attempts[i].state == contracts.AttemptRunning {
			w.attempts[i].state, w.attempts[i].reason = contracts.AttemptErrored, contracts.ReasonCancelled
		}
		if w.attempts[i].sandbox != "" {
			w.attempts[i].released = true
		}
	}
	if w.task != "" && w.task.CanMoveTo(contracts.TaskCancelled) {
		w.task = contracts.TaskCancelled
		w.taskRev++
	}
	return nil
}

func (w *world) Deliver(_ context.Context, attempt, hash, base string) (activity.DeliverResult, error) {
	if err := w.count("Deliver"); err != nil {
		return activity.DeliverResult{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delivers++
	return activity.DeliverResult{ArtifactHash: hash, BaseSHA: base, Detail: activity.DetailDeliveryStub}, nil
}

// deliveryStep counts a delivery activity and returns its error or its
// next queued stop. Its caller holds no lock.
func (w *world) deliveryStep(name string) (activity.DeliveryStep, error) {
	if err := w.count(name); err != nil {
		return activity.DeliveryStep{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	q := w.stops[name]
	if len(q) == 0 {
		return activity.DeliveryStep{}, nil
	}
	w.stops[name] = q[1:]
	return q[0], nil
}

// WritePullRequest calls the model only when no row is written yet, as
// the real activity returns the recorded row.
func (w *world) WritePullRequest(_ context.Context, in activity.DeliveryInput) (activity.DeliveryStep, error) {
	out, err := w.deliveryStep("WritePullRequest")
	if err != nil || out.Stop != "" {
		return out, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if in.AttemptID == "" || in.TaskID == "" || in.BaseSHA != baseSHA || in.Issue != issueNum {
		return out, temporal.NewNonRetryableApplicationError("bad delivery input", activity.ErrTypePermanent, nil)
	}
	if !w.pr.written {
		w.pr.written = true
		w.pr.modelCalls++
	}
	return out, nil
}

// Land pushes the integration branch once.
func (w *world) Land(_ context.Context, _ activity.DeliveryInput) (activity.DeliveryStep, error) {
	out, err := w.deliveryStep("Land")
	if err != nil || out.Stop != "" {
		return out, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.pr.written {
		return out, temporal.NewNonRetryableApplicationError("no pull request row", activity.ErrTypePermanent, nil)
	}
	if !w.pr.landed {
		w.pr.landed = true
		w.pr.landings++
	}
	return out, nil
}

// Finalize records the rebase; a queued stop leaves it unrecorded, as a
// conflict or a failed gate does.
func (w *world) Finalize(_ context.Context, _ activity.DeliveryInput) (activity.DeliveryStep, error) {
	out, err := w.deliveryStep("Finalize")
	if err != nil || out.Stop != "" {
		return out, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.pr.landed {
		return out, temporal.NewNonRetryableApplicationError("not landed", activity.ErrTypePermanent, nil)
	}
	w.pr.finalized = true
	return out, nil
}

// OpenPullRequest opens the draft once and reports how to wait for CI.
func (w *world) OpenPullRequest(_ context.Context, _ activity.DeliveryInput) (activity.OpenResult, error) {
	if err := w.count("OpenPullRequest"); err != nil {
		return activity.OpenResult{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.pr.finalized {
		return activity.OpenResult{}, temporal.NewNonRetryableApplicationError("not finalized", activity.ErrTypePermanent, nil)
	}
	if !w.pr.opened {
		w.pr.opened = true
		w.pr.opens++
	}
	ci := w.ci
	if ci == (activity.CIWait{}) {
		ci = activity.CIWait{PollEvery: time.Minute, Timeout: time.Hour}
	}
	return activity.OpenResult{Number: 1, URL: "https://github.example/pull/1", CI: ci}, nil
}

// ReadChecks answers from w.checks.
func (w *world) ReadChecks(_ context.Context, _ activity.DeliveryInput) (activity.ChecksResult, error) {
	if err := w.count("ReadChecks"); err != nil {
		return activity.ChecksResult{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.pr.opened {
		return activity.ChecksResult{}, temporal.NewNonRetryableApplicationError("no pull request", activity.ErrTypePermanent, nil)
	}
	w.pr.reads++
	if w.checks == nil {
		return activity.ChecksResult{Completed: true, Passed: true}, nil
	}
	return w.checks(w.pr.reads), nil
}

// MarkReady marks the draft ready once.
func (w *world) MarkReady(_ context.Context, _ activity.DeliveryInput) (activity.DeliveryStep, error) {
	out, err := w.deliveryStep("MarkReady")
	if err != nil || out.Stop != "" {
		return out, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.pr.opened {
		return out, temporal.NewNonRetryableApplicationError("no pull request", activity.ErrTypePermanent, nil)
	}
	if !w.pr.ready {
		w.pr.ready = true
		w.pr.readies++
	}
	return out, nil
}

// delivery returns a copy of the delivery state.
func (w *world) delivery() fakeDelivery {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pr
}

func (w *world) UpsertStatusComment(_ context.Context, in activity.StatusCommentInput) error {
	if err := w.count("UpsertStatusComment"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == nil {
		w.statusPosts++
	} else {
		w.statusEdits++
	}
	v := in.View
	w.status = &v
	return nil
}

// RecordCommand is keyed on the comment ID, as the table's unique key is.
func (w *world) RecordCommand(_ context.Context, in activity.RecordCommandInput) error {
	if err := w.count("RecordCommand"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.audit[in.Ref.CommentID]; !ok {
		w.audit[in.Ref.CommentID] = in
		w.auditOrder = append(w.auditOrder, in.Ref.CommentID)
	}
	return nil
}

func (w *world) ReplyToCommand(_ context.Context, in activity.ReplyInput) error {
	if err := w.count("ReplyToCommand"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.replies[in.Ref.CommentID]; !ok {
		w.replies[in.Ref.CommentID] = in
	}
	return nil
}

func (w *world) CheckCaps(_ context.Context, _ string) (activity.CapCheck, error) {
	if err := w.count("CheckCaps"); err != nil {
		return activity.CapCheck{}, err
	}
	if w.duringCheckCaps != nil {
		w.duringCheckCaps()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.caps, nil
}

// ResumeTask reopens the task and returns one past the highest ordinal,
// as the real activity does.
func (w *world) ResumeTask(_ context.Context, in activity.ResumeTaskInput) (activity.ResumeTaskResult, error) {
	if err := w.count("ResumeTask"); err != nil {
		return activity.ResumeTaskResult{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch w.task {
	case contracts.TaskNeedsAttention:
		w.task = contracts.TaskRunning
		w.taskRev++
	case contracts.TaskRunning:
	default:
		return activity.ResumeTaskResult{}, temporal.NewNonRetryableApplicationError("task cannot resume", activity.ErrTypePermanent, nil)
	}
	return activity.ResumeTaskResult{Ordinal: len(w.attempts) + 1}, nil
}

func (w *world) LookupFeature(_ context.Context, in activity.LookupFeatureInput) (activity.LookupFeatureResult, error) {
	if err := w.count("LookupFeature"); err != nil {
		return activity.LookupFeatureResult{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	res := w.lookup
	if row, ok := w.audit[in.CommentID]; ok {
		res.RecordedBy = row.Ref.WorkflowID
	}
	return res, nil
}

// auditRow returns the audit row for a comment ID.
func (w *world) auditRow(id int64) (activity.RecordCommandInput, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.audit[id]
	return r, ok
}

// reply returns the reply to a comment ID.
func (w *world) reply(id int64) (activity.ReplyInput, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.replies[id]
	return r, ok
}

// register puts every world method in env under the activity's name, so
// the workflow's calls through acts reach the fake.
func (w *world) register(env *testsuite.TestWorkflowEnvironment) {
	w.registerOn(env)
}

// registrar is a test environment or a worker.
type registrar interface {
	RegisterActivityWithOptions(a any, options sdkactivity.RegisterOptions)
}

func (w *world) registerOn(r registrar) {
	fakes := []struct {
		name string
		fn   any
	}{
		{"Analyze", w.Analyze}, {"PostPlanComment", w.PostPlanComment}, {"Transition", w.Transition},
		{"Approve", w.Approve}, {"CreateTask", w.CreateTask}, {"LoadTask", w.LoadTask},
		{"PrepareAttempt", w.PrepareAttempt}, {"RunAttempt", w.RunAttempt}, {"SettleAttempt", w.SettleAttempt},
		{"ReleaseAttemptSandbox", w.ReleaseAttemptSandbox}, {"ReleaseFeatureSandboxes", w.ReleaseFeatureSandboxes},
		{"CancelTask", w.CancelTask}, {"Deliver", w.Deliver},
		{"UpsertStatusComment", w.UpsertStatusComment}, {"RecordCommand", w.RecordCommand},
		{"ReplyToCommand", w.ReplyToCommand}, {"CheckCaps", w.CheckCaps}, {"ResumeTask", w.ResumeTask},
		{"LookupFeature", w.LookupFeature},
		{"WritePullRequest", w.WritePullRequest}, {"Land", w.Land}, {"Finalize", w.Finalize},
		{"OpenPullRequest", w.OpenPullRequest}, {"ReadChecks", w.ReadChecks}, {"MarkReady", w.MarkReady},
	}
	for _, f := range fakes {
		r.RegisterActivityWithOptions(f.fn, sdkactivity.RegisterOptions{Name: f.name})
	}
}

// newEnv returns a test environment with both workflows and w's
// activities registered.
func newEnv(t *testing.T, w *world) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(FeatureWorkflow)
	env.RegisterWorkflow(TaskWorkflow)
	env.RegisterWorkflow(ClosedCommandWorkflow)
	w.register(env)
	env.SetTestTimeout(30 * time.Second)
	return env
}

func featureInput() FeatureInput {
	return FeatureInput{
		OwnerUserID: ownerID,
		Start: github.FeatureStart{
			WorkflowID: github.FeatureWorkflowID(repoID, issueNum),
			Repo:       github.RepoRef{ID: repoID, FullName: "austmsk/election-tally", InstallationID: 5},
			Issue:      github.IssueSnapshot{Number: issueNum, Title: "Add export", Body: "## Task\nx", Author: github.Author{ID: ownerID}},
		},
	}
}

// command is an owner command signal with the given comment ID.
func command(name github.CommandName, commentID int64) github.CommandSignal {
	return github.CommandSignal{
		WorkflowID: github.FeatureWorkflowID(repoID, issueNum), IssueNumber: issueNum,
		CommentID: commentID, ActorID: ownerID, Command: github.Command{Name: name},
	}
}

// signalAfter sends a command once d of workflow time has passed.
func signalAfter(env *testsuite.TestWorkflowEnvironment, d time.Duration, sig github.CommandSignal) {
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalName(sig.Command.Name), sig)
	}, d)
}

// approveThenCancel approves after a minute and, if the workflow is still
// open after a day, cancels it, so a run that stops in needs_attention
// completes.
func approveThenCancel(env *testsuite.TestWorkflowEnvironment) {
	signalAfter(env, time.Minute, command(github.CommandApprove, 100))
	signalAfter(env, 24*time.Hour, command(github.CommandCancel, 101))
}

// queryStatus reads the workflow's status query.
func queryStatus(t *testing.T, env *testsuite.TestWorkflowEnvironment) Status {
	t.Helper()
	v, err := env.QueryWorkflow(QueryStatus)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var st Status
	if err := v.Get(&st); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	return st
}

// returns is a RunAttempt behaviour that records state and reason and
// returns the outcome as a value.
func returns(state contracts.AttemptState, reason contracts.FailureReason) func(context.Context, *world, int) (activity.AttemptOutcome, error) {
	return func(_ context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		return w.finish(n, state, reason), nil
	}
}

// blocksUntilCancelled is a RunAttempt behaviour that calls during once
// it has started, then waits for its cancellation and records it, as the
// real attempt does.
func blocksUntilCancelled(during func()) func(context.Context, *world, int) (activity.AttemptOutcome, error) {
	return func(ctx context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		during()
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Second):
			return activity.AttemptOutcome{}, fmt.Errorf("attempt was never cancelled")
		}
		w.finish(n, contracts.AttemptErrored, contracts.ReasonCancelled)
		return activity.AttemptOutcome{}, ctx.Err()
	}
}

func equal[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func equalSlices(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

// sdkRegister replaces the fake registered under name.
func sdkRegister(name string) sdkactivity.RegisterOptions {
	return sdkactivity.RegisterOptions{Name: name, DisableAlreadyRegisteredCheck: true}
}
