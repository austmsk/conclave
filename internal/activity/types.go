package activity

import (
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
)

// Activity inputs and results. Every field is a small serializable value:
// IDs, revisions, states, reasons and hashes. Diffs, transcripts and tool
// output never cross the activity boundary, because whatever crosses it is
// written into workflow history (RL-14); activities pass blob keys and
// hashes instead.

// Feature request reasons, the kind before any ":detail". A feature
// request in needs_attention carries one of these as its reason, with a
// detail after a colon where there is one: budget_cap:<cap kind>,
// error:<what went wrong>, or attempt_failed alone.
const (
	ReasonBudgetCap     = "budget_cap"
	ReasonAttemptFailed = "attempt_failed"
	ReasonError         = "error"

	// Delivery stops (decision 0014). The pull request, if one was
	// opened, stays open as a draft; /retry resumes delivery where the
	// pull_requests row says it stopped.
	//
	// ReasonRebaseConflict: the attempt's diff does not apply to the
	// default branch's new head. FR-S19 reruns the task there; Milestone
	// 1 stops instead, because the plan's base is pinned write-once, and
	// /retry meets the same conflict, so /cancel is the way out until
	// Milestone 4.
	ReasonRebaseConflict = "rebase_conflict"
	// ReasonRebaseGateFailed: the rebased artifact, a new artifact, failed
	// a gate (FR-SEC27).
	ReasonRebaseGateFailed = "rebase_gate_failed"
	// ReasonCIFailed: a required check completed without passing.
	ReasonCIFailed = "ci_failed"
	// ReasonCITimeout: the required checks had not all completed when the
	// repository's CI timeout passed.
	ReasonCITimeout = "ci_timeout"
)

// FeatureReason formats a feature request's reason: kind alone when detail
// is empty, otherwise kind:detail.
func FeatureReason(kind, detail string) string {
	if detail == "" {
		return kind
	}
	return kind + ":" + detail
}

// DetailDeliveryStub is the detail the ticket 8 deliver stub stops with.
const DetailDeliveryStub = "delivery arrives in ticket 9"

// AnalyzeInput is the issue as the signed labeled event carried it.
type AnalyzeInput struct {
	WorkflowID string
	Repo       github.RepoRef
	Issue      github.IssueSnapshot
}

// AnalyzeResult names the feature request and, when the issue could be
// planned, its first plan. Problem is set instead of a plan when the
// workflow must stop in needs_attention: the issue's author is not the
// owner, or a required section is missing.
type AnalyzeResult struct {
	FeatureRequestID string
	RepoID           string
	Status           contracts.FeatureRequestStatus
	Revision         int

	PlanID       string
	PlanRevision int
	PlanStatus   contracts.PlanStatus

	Problem string
}

// PlanCommentInput names the plan whose comment to post.
type PlanCommentInput struct {
	Repo   github.RepoRef
	Issue  int
	PlanID string
}

// PlanCommentResult is the posted comment. Created is false when a
// comment carrying the plan's marker was already on the issue.
type PlanCommentResult struct {
	CommentID int64
	Created   bool
}

// TransitionInput is one compare-and-set state change of a feature
// request or a task. Attempts change state through FinishAttempt and plans
// through Approve.
type TransitionInput struct {
	Kind     contracts.EntityKind
	ID       string
	From     string
	To       string
	Revision int
	Reason   string
	Actor    string
}

// ApproveInput pins the base commit and starts execution.
type ApproveInput struct {
	Repo             github.RepoRef
	FeatureRequestID string
	FeatureRevision  int
	PlanID           string
	PlanRevision     int
	Actor            string
}

// ApproveResult is the pinned base commit and the new revisions.
type ApproveResult struct {
	BaseSHA         string
	FeatureRevision int
	PlanRevision    int
}

// TaskRef is a task's identity and state.
type TaskRef struct {
	TaskID   string
	Key      string
	Status   contracts.TaskStatus
	Revision int
}

// PrepareAttemptInput names the attempt to create.
type PrepareAttemptInput struct {
	TaskID  string
	Ordinal int
	Role    contracts.Role
}

// AttemptRef is an attempt row as stored, plus the StartToCloseTimeout
// RunAttempt must be given. The timeout is computed here from
// configuration and recorded in history, so a configuration change cannot
// change it for a run already in flight (RL-16).
type AttemptRef struct {
	AttemptID    string
	Ordinal      int
	Revision     int
	State        contracts.AttemptState
	Reason       contracts.FailureReason
	ArtifactHash string
	Timeout      time.Duration
}

// RunAttemptInput is one attempt. It names the repository by full name
// only; the tree source finds the rest in the plan body.
type RunAttemptInput struct {
	AttemptID    string
	TaskID       string
	BaseSHA      string
	RepoFullName string
	Role         contracts.Role
}

// AttemptOutcome is how an attempt ended, as recorded.
type AttemptOutcome struct {
	AttemptID    string
	State        contracts.AttemptState
	Reason       contracts.FailureReason
	ArtifactHash string
}

// SettleInput ends an attempt the attempt activity could not finish.
type SettleInput struct {
	AttemptID string
	Reason    contracts.FailureReason
	Actor     string
}

// CancelTaskInput cancels a task and everything still open under it.
type CancelTaskInput struct {
	TaskID string
	Actor  string
}

// DeliverResult is what delivery did. The ticket 8 stub never delivers:
// it verifies the artifact it would push and reports Detail.
type DeliverResult struct {
	Delivered    bool
	ArtifactHash string
	BaseSHA      string
	Detail       string
}

// DeliveryInput names the passed attempt to deliver and where. Every
// delivery activity takes it; each reads the pull_requests row for the
// rest, so a retry or a /retry resumes from what is recorded.
type DeliveryInput struct {
	AttemptID    string
	TaskID       string
	ArtifactHash string
	// BaseSHA is the plan's pinned base, the one the attempt ran on.
	BaseSHA string
	Repo    github.RepoRef
	Issue   int
}

// DeliveryStep is what one delivery step did. Stop is empty when the step
// finished and delivery goes on; otherwise it is the reason kind the
// feature request stops in needs_attention with (ReasonRebaseConflict,
// ReasonRebaseGateFailed, ReasonCIFailed), and Detail says why.
type DeliveryStep struct {
	Stop   string
	Detail string
}

// CIWait is how the workflow waits for CI: poll every PollEvery until
// Timeout has passed. It is read from configuration by OpenPullRequest
// and recorded in history, so a configuration change cannot change it
// for a run in flight (RL-16).
type CIWait struct {
	PollEvery time.Duration
	Timeout   time.Duration
}

// OpenResult is the opened draft pull request and how to wait for its CI.
type OpenResult struct {
	Number int
	URL    string
	CI     CIWait
}

// ChecksResult is one read of the pull request's required checks.
// Completed is set when every required check has completed, or one has
// completed without passing; Passed when every one passed. A completed
// read is recorded in pr_ci_results.
type ChecksResult struct {
	Completed bool
	Passed    bool
	Detail    string
}

// CommandRef identifies one owner command for its audit row and its
// reply: the signal's identity without the command's free text, which
// neither needs.
type CommandRef struct {
	CommentID  int64
	DeliveryID string
	WorkflowID string
	Repo       github.RepoRef
	Issue      int
	Command    github.CommandName
	ActorID    int64
}

// CommandRefOf is the identity of the command a signal carries.
func CommandRefOf(sig github.CommandSignal) CommandRef {
	return CommandRef{
		CommentID: sig.CommentID, DeliveryID: sig.DeliveryID, WorkflowID: sig.WorkflowID,
		Repo: sig.Repo, Issue: sig.IssueNumber, Command: sig.Command.Name, ActorID: sig.ActorID,
	}
}

// Command outcomes and rejection reasons, as the commands audit records
// them.
const (
	CommandAccepted = store.CommandAccepted
	CommandRejected = store.CommandRejected

	// RejectNotValid: the command table does not list the command in the
	// feature request's status.
	RejectNotValid = "not_valid_in_status"
	// RejectNotAvailable: /revise and /ask are in the table, but no
	// Milestone 1 ticket implements them.
	RejectNotAvailable = "not_available"
	// RejectAlreadyPending: an /approve or /cancel arrived while an
	// earlier one was still being acted on.
	RejectAlreadyPending = "already_pending"
	// RejectOverCap prefixes a /retry refused for a cap: over_cap:<kind>.
	RejectOverCap = "over_cap"
	// RejectResumeFailed: /retry passed the cap check, but the caps could
	// not be read or the move out of needs_attention failed.
	RejectResumeFailed = "resume_failed"
	// RejectCancelled: a /retry overtaken by a /cancel, or by the run's
	// end.
	RejectCancelled = "cancelled"
	// RejectClosedRun: the run is delivered or cancelled.
	RejectClosedRun = "closed_run"
	// RejectNoRun: the issue has no feature request.
	RejectNoRun = "no_run"
	// RejectNoWorkflow: the feature request is live, but no workflow is
	// running for it.
	RejectNoWorkflow = "no_workflow"
)

// RecordCommandInput is one command's audit row.
type RecordCommandInput struct {
	Ref              CommandRef
	FeatureRequestID string
	Status           contracts.FeatureRequestStatus
	Outcome          string
	Reason           string
}

// ReplyKind says what a command reply reports.
type ReplyKind string

const (
	ReplyNotValid     ReplyKind = "not_valid"
	ReplyNotAvailable ReplyKind = "not_available"
	ReplyStatus       ReplyKind = "status"
	ReplyHelp         ReplyKind = "help"
	ReplyOverCap      ReplyKind = "over_cap"
	ReplyResumeFailed ReplyKind = "resume_failed"
	ReplyClosedRun    ReplyKind = "closed_run"
	ReplyNoRun        ReplyKind = "no_run"
	ReplyNoWorkflow   ReplyKind = "no_workflow"
	// ReplyAlreadyPending answers an /approve or /cancel sent while an
	// earlier one is still being acted on.
	ReplyAlreadyPending ReplyKind = "already_pending"
	// ReplyCancelled answers a /retry that a /cancel overtook.
	ReplyCancelled ReplyKind = "cancelled"
)

// RunView is the feature request as the owner is shown it: its status,
// the reason and the phase it left when it is in needs_attention, and the
// commands the command table allows in that status, in the table's order.
type RunView struct {
	FeatureRequestID string
	Status           contracts.FeatureRequestStatus
	Reason           string
	PhaseLeft        contracts.FeatureRequestStatus
	Commands         []github.CommandName
	// Cap is set when a /retry was refused for a cap still exceeded.
	Cap *CapCheck
}

// ReplyInput is one reply to a command. The reply is rendered by the
// activity, from these values, so the workflow decides what to say and
// never the words.
type ReplyInput struct {
	Ref    CommandRef
	Kind   ReplyKind
	View   RunView
	Detail string
}

// StatusCommentInput is the status comment's content and where it goes.
type StatusCommentInput struct {
	Repo  github.RepoRef
	Issue int
	View  RunView
}

// CapCheck is the feature request's spend against each platform cap, and
// the first cap at or over its limit, if any.
type CapCheck struct {
	Exceeded             llm.BudgetKind
	FeatureRequestTokens int
	FeatureRequestCap    int
	MonthUSD             float64
	MonthCap             float64
}

// ResumeTaskInput reopens a task in needs_attention for a /retry.
type ResumeTaskInput struct {
	TaskID string
	Actor  string
}

// ResumeTaskResult is the ordinal the task's next attempt takes.
type ResumeTaskResult struct {
	Ordinal int
}

// LookupFeatureInput names the issue's feature workflow and the command
// a closed run was asked to answer.
type LookupFeatureInput struct {
	WorkflowID string
	CommentID  int64
}

// LookupFeatureResult is the feature request recorded under a workflow
// ID, if any, and the workflow that recorded the command, if it is
// already in the audit.
type LookupFeatureResult struct {
	Found            bool
	FeatureRequestID string
	Status           contracts.FeatureRequestStatus
	Reason           string
	RecordedBy       string
}
