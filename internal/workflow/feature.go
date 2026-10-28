package workflow

import (
	"errors"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/store"
)

// errStopped ends a phase whose problem has been routed: the feature
// request is in needs_attention or cancelled, and the workflow waits or
// ends.
var errStopped = errors.New("feature request stopped")

// FeatureWorkflow runs one issue: analyze it into a plan, post the plan,
// wait for /approve, run the task as a child workflow, and deliver. Every
// problem routes the feature request to needs_attention with a reason,
// never to an automatic failed status; the workflow then waits, and in
// ticket 8 only /cancel is handled there. A /cancel, or the workflow's
// own cancellation, moves the feature request to cancelled after the
// child has released its sandbox.
//
// It returns nil when the feature request reaches cancelled through
// /cancel, and the cancellation when Temporal cancelled it.
func FeatureWorkflow(ctx workflow.Context, in FeatureInput) error {
	f := &featureRun{
		in:    in,
		log:   workflow.GetLogger(ctx),
		actor: store.ActorWorkflow(workflow.GetInfo(ctx).WorkflowExecution.ID),
		st:    Status{Status: contracts.FeaturePlanning},
	}
	if err := workflow.SetQueryHandler(ctx, QueryStatus, func() (Status, error) { return f.st, nil }); err != nil {
		return err
	}
	f.listen(ctx)
	defer f.sweep(ctx)

	err := f.run(ctx)
	if err == nil {
		err = f.awaitCancel(ctx)
	}
	if ctx.Err() != nil {
		_ = f.closeCancelled(cleanupContext(ctx))
		return ctx.Err()
	}
	if errors.Is(err, errStopped) {
		return nil
	}
	return err
}

// featureRun is one FeatureWorkflow in progress. Its fields are read by
// the signal handlers and the status query.
type featureRun struct {
	in    FeatureInput
	log   log.Logger
	actor string

	st      Status
	frRev   int
	planRev int

	approval *github.CommandSignal
	cancel   *github.CommandSignal
}

// run takes the feature request from planning to the point where it
// waits: needs_attention after delivery or a problem. It returns
// errStopped once the feature request is cancelled.
func (f *featureRun) run(ctx workflow.Context) error {
	steps := []func(workflow.Context) error{f.analyze, f.awaitApproval, f.approve, f.execute}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return f.handle(ctx, err)
		}
		if f.cancel != nil {
			return f.closeCancelled(ctx)
		}
	}
	return nil
}

// handle routes a step's error. A cancellation is left to the caller,
// errStopped passes through, and anything else moves the feature request
// to needs_attention with reason error.
func (f *featureRun) handle(ctx workflow.Context, err error) error {
	switch {
	case ctx.Err() != nil, errors.Is(err, errStopped):
		return err
	case f.cancel != nil:
		return f.closeCancelled(ctx)
	}
	f.log.Error("feature workflow step failed", "status", string(f.st.Status), "error", err)
	f.needsAttention(ctx, activity.FeatureReason(activity.ReasonError, errorDetail(err)))
	return nil
}

// analyze records the feature request and its plan, moves it to
// awaiting_approval, and posts the plan comment, so /approve is never
// blind. An issue that cannot be planned stops in needs_attention.
func (f *featureRun) analyze(ctx workflow.Context) error {
	actx := smallOptions(ctx)
	var res activity.AnalyzeResult
	err := workflow.ExecuteActivity(actx, acts.Analyze, activity.AnalyzeInput{
		WorkflowID: f.in.Start.WorkflowID, Repo: f.in.Start.Repo, Issue: f.in.Start.Issue,
	}).Get(ctx, &res)
	if err != nil {
		return err
	}
	f.st.FeatureRequestID, f.st.PlanID = res.FeatureRequestID, res.PlanID
	f.st.Status, f.frRev, f.planRev = res.Status, res.Revision, res.PlanRevision
	if res.Status != contracts.FeaturePlanning {
		f.log.Warn("feature request is past planning; the workflow does not drive it", "status", string(res.Status))
		return errStopped
	}
	if res.Problem != "" {
		f.needsAttention(ctx, activity.FeatureReason(activity.ReasonError, res.Problem))
		return nil
	}
	if err := f.transition(ctx, contracts.FeatureAwaitingApproval, ""); err != nil {
		return err
	}
	return workflow.ExecuteActivity(actx, acts.PostPlanComment, activity.PlanCommentInput{
		Repo: f.in.Start.Repo, Issue: f.in.Start.Issue.Number, PlanID: res.PlanID,
	}).Get(ctx, nil)
}

// awaitApproval waits, with no timeout, for /approve or /cancel. The
// wait costs nothing while it lasts: the workflow holds no timer and no
// worker slot.
func (f *featureRun) awaitApproval(ctx workflow.Context) error {
	if f.st.Status != contracts.FeatureAwaitingApproval {
		return nil
	}
	return workflow.Await(ctx, func() bool { return f.approval != nil || f.cancel != nil })
}

// approve pins the base commit and moves the feature request to
// executing, in one activity.
func (f *featureRun) approve(ctx workflow.Context) error {
	if f.approval == nil || f.cancel != nil {
		return nil
	}
	var res activity.ApproveResult
	err := workflow.ExecuteActivity(smallOptions(ctx), acts.Approve, activity.ApproveInput{
		Repo: f.in.Start.Repo, FeatureRequestID: f.st.FeatureRequestID, FeatureRevision: f.frRev,
		PlanID: f.st.PlanID, PlanRevision: f.planRev, Actor: store.ActorGitHub(f.approval.ActorID),
	}).Get(ctx, &res)
	if err != nil {
		return err
	}
	f.st.Status, f.st.BaseSHA, f.frRev, f.planRev = contracts.FeatureExecuting, res.BaseSHA, res.FeatureRevision, res.PlanRevision
	return nil
}

// execute creates the task, runs it as a child workflow, and routes its
// result.
func (f *featureRun) execute(ctx workflow.Context) error {
	if f.st.Status != contracts.FeatureExecuting {
		return nil
	}
	var task activity.TaskRef
	if err := workflow.ExecuteActivity(smallOptions(ctx), acts.CreateTask, f.st.PlanID).Get(ctx, &task); err != nil {
		return err
	}
	f.st.TaskID = task.TaskID
	res, err := f.runTask(ctx)
	if err != nil || f.cancel != nil {
		return err
	}
	return f.routeTask(ctx, res)
}

// runTask runs the child and waits for it, or for /cancel, which cancels
// the child. The child is awaited on a context that survives
// cancellation, so the feature request is not closed until the child has
// recorded its attempt and released its sandbox.
func (f *featureRun) runTask(ctx workflow.Context) (TaskResult, error) {
	childCtx, cancelChild := workflow.WithCancel(ctx)
	childCtx = workflow.WithChildOptions(childCtx, workflow.ChildWorkflowOptions{
		WorkflowID:          f.in.Start.WorkflowID + "/task/" + f.st.TaskID,
		ParentClosePolicy:   enumspb.PARENT_CLOSE_POLICY_TERMINATE,
		WaitForCancellation: true,
	})
	fut := workflow.ExecuteChildWorkflow(childCtx, TaskWorkflow, TaskInput{
		TaskID: f.st.TaskID, Ordinal: 1, BaseSHA: f.st.BaseSHA, Repo: f.in.Start.Repo, Role: contracts.RoleImplementer,
	})
	var res TaskResult
	var childErr error
	done := false
	waitCtx, _ := workflow.NewDisconnectedContext(ctx)
	workflow.Go(waitCtx, func(gctx workflow.Context) {
		childErr = fut.Get(gctx, &res)
		done = true
	})
	err := workflow.Await(ctx, func() bool { return done || f.cancel != nil })
	if !done {
		cancelChild()
		_ = workflow.Await(waitCtx, func() bool { return done })
	}
	if err != nil {
		return res, err
	}
	if childErr != nil && f.cancel == nil {
		return res, childErr
	}
	return res, nil
}

// routeTask moves the feature request on from a finished task: a passed
// task is delivered, and anything else stops in needs_attention with its
// reason.
func (f *featureRun) routeTask(ctx workflow.Context, res TaskResult) error {
	switch res.Outcome {
	case TaskPassed:
		return f.deliver(ctx, res)
	case TaskFailed:
		f.needsAttention(ctx, activity.ReasonAttemptFailed)
	case TaskBudgetCap:
		f.needsAttention(ctx, activity.FeatureReason(activity.ReasonBudgetCap, string(res.BudgetKind)))
	default:
		detail := res.Detail
		if detail == "" {
			detail = "attempts errored: " + string(res.Reason)
		}
		f.needsAttention(ctx, activity.FeatureReason(activity.ReasonError, detail))
	}
	return nil
}

// deliver runs the deliver stub, which verifies the artifact and reports
// that delivery arrives in ticket 9; the feature request stops in
// needs_attention with that detail, never delivered.
func (f *featureRun) deliver(ctx workflow.Context, res TaskResult) error {
	var out activity.DeliverResult
	err := workflow.ExecuteActivity(smallOptions(ctx), acts.Deliver, res.AttemptID, res.ArtifactHash, f.st.BaseSHA).Get(ctx, &out)
	if err != nil {
		return err
	}
	if out.Delivered {
		return f.transition(ctx, contracts.FeatureDelivered, "")
	}
	f.needsAttention(ctx, activity.FeatureReason(activity.ReasonError, out.Detail))
	return nil
}

// awaitCancel is the needs_attention wait: only /cancel is handled here
// in ticket 8. It also holds a feature request whose move to
// needs_attention failed, so the workflow never ends with the record
// open. Only a terminal feature request has nothing to wait for.
func (f *featureRun) awaitCancel(ctx workflow.Context) error {
	if !f.st.Status.CanMoveTo(contracts.FeatureCancelled) {
		return nil
	}
	if err := workflow.Await(ctx, func() bool { return f.cancel != nil }); err != nil {
		return err
	}
	return f.closeCancelled(ctx)
}

// needsAttention makes the compare-and-set move to needs_attention with
// reason, recording the phase it left in workflow state for ticket 8b's
// /retry. awaiting_approval cannot move to needs_attention, so it steps
// back to planning first: the plan is unapproved, and /retry must not
// skip /approve (FR-S5). If the move itself fails for good, the workflow
// logs it and waits all the same: it must not fail.
func (f *featureRun) needsAttention(ctx workflow.Context, reason string) {
	if f.st.Status == contracts.FeatureAwaitingApproval {
		if err := f.transition(ctx, contracts.FeaturePlanning, ""); err != nil {
			f.log.Error("stepping back to planning before needs_attention", "error", err)
		}
	}
	left := f.st.Status
	if err := f.transition(ctx, contracts.FeatureNeedsAttention, reason); err != nil {
		f.log.Error("moving feature request to needs_attention", "reason", reason, "error", err)
		return
	}
	f.st.PhaseLeft, f.st.Reason = left, reason
}

// closeCancelled cancels the task, if there is one, and moves the
// feature request to cancelled. ctx is the workflow's own context after
// /cancel and a disconnected one after the workflow was cancelled.
func (f *featureRun) closeCancelled(ctx workflow.Context) error {
	if f.st.FeatureRequestID == "" || f.st.Status == contracts.FeatureCancelled {
		return errStopped
	}
	actx := smallOptions(ctx)
	if f.st.TaskID != "" {
		if err := workflow.ExecuteActivity(actx, acts.CancelTask, activity.CancelTaskInput{
			TaskID: f.st.TaskID, Actor: f.cancelActor(),
		}).Get(ctx, nil); err != nil {
			f.log.Error("cancelling task", "task_id", f.st.TaskID, "error", err)
		}
	}
	if err := f.transitionAs(ctx, contracts.FeatureCancelled, "", f.cancelActor()); err != nil {
		f.log.Error("moving feature request to cancelled", "error", err)
	}
	f.st.Status = contracts.FeatureCancelled
	return errStopped
}

// cancelActor attributes a cancellation to the owner who sent /cancel,
// or to the workflow when Temporal cancelled it.
func (f *featureRun) cancelActor() string {
	if f.cancel != nil {
		return store.ActorGitHub(f.cancel.ActorID)
	}
	return f.actor
}

// transition makes one compare-and-set change of the feature request as
// the workflow.
func (f *featureRun) transition(ctx workflow.Context, to contracts.FeatureRequestStatus, reason string) error {
	return f.transitionAs(ctx, to, reason, f.actor)
}

func (f *featureRun) transitionAs(ctx workflow.Context, to contracts.FeatureRequestStatus, reason, actor string) error {
	var rev int
	err := workflow.ExecuteActivity(smallOptions(ctx), acts.Transition, activity.TransitionInput{
		Kind: contracts.EntityFeatureRequest, ID: f.st.FeatureRequestID, From: string(f.st.Status), To: string(to),
		Revision: f.frRev, Reason: reason, Actor: actor,
	}).Get(ctx, &rev)
	if err != nil {
		return err
	}
	f.st.Status, f.frRev = to, rev
	return nil
}

// sweep releases, from a disconnected context, every sandbox of the
// feature request still recorded and unreleased as the workflow ends: a
// net behind each attempt's own release.
func (f *featureRun) sweep(ctx workflow.Context) {
	if f.st.FeatureRequestID == "" {
		return
	}
	cctx := cleanupContext(ctx)
	if err := workflow.ExecuteActivity(cctx, acts.ReleaseFeatureSandboxes, f.st.FeatureRequestID).Get(cctx, nil); err != nil {
		f.log.Error("sweeping feature request sandboxes", "error", err)
	}
}
