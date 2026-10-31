package activity

import (
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
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
