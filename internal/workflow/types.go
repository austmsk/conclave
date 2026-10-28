package workflow

import (
	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
)

// Task queues. Workflows and the small activities share TaskQueue;
// RunAttempt runs on AttemptTaskQueue, which the worker polls with one
// slot, so two issues labelled at once queue their attempts.
const (
	TaskQueue        = "conclave"
	AttemptTaskQueue = "conclave-attempt"
)

// QueryStatus names the query that returns the feature workflow's Status.
const QueryStatus = "status"

// MaxErroredAttempts is how many errored attempts a task takes before it
// stops: the first errored attempt gets a new ordinal, the second moves
// the task and the feature request to needs_attention.
const MaxErroredAttempts = 2

// maxRecentComments bounds each command's set of seen comment IDs.
const maxRecentComments = 64

// FeatureInput starts a feature workflow: the issue as the signed labeled
// event carried it, and the owner whose commands count.
type FeatureInput struct {
	Start       github.FeatureStart
	OwnerUserID int64
}

// SignalName is the signal a command arrives on: the command's own name,
// so a signal names exactly one command.
func SignalName(name github.CommandName) string {
	return string(name)
}

// commandNames is every command the receiver listens for, in a fixed
// order. Only approve and cancel act in ticket 8; the rest are logged and
// dropped until ticket 8b replies to them.
var commandNames = []github.CommandName{
	github.CommandApprove, github.CommandCancel, github.CommandRevise, github.CommandAsk,
	github.CommandStatus, github.CommandRetry, github.CommandHelp,
}

// Status is the feature workflow's view of its feature request, returned
// by QueryStatus. PhaseLeft is the status the feature request left for
// needs_attention: ticket 8b's /retry returns to it. It lives here, not
// in Postgres; state_transitions.from_state records it durably.
type Status struct {
	Status           contracts.FeatureRequestStatus
	Reason           string
	PhaseLeft        contracts.FeatureRequestStatus
	FeatureRequestID string
	PlanID           string
	TaskID           string
	BaseSHA          string
}

// TaskInput starts a task workflow. Every field is serializable and none
// is GitHub state the child would have to fetch: Milestone 2's benchmark
// starts the workflow directly. Repo carries the full name, and the
// numeric ID and installation the clone needs.
type TaskInput struct {
	TaskID  string
	Ordinal int
	BaseSHA string
	Repo    github.RepoRef
	Role    contracts.Role
}

// TaskOutcome is how a task workflow ended.
type TaskOutcome string

const (
	TaskPassed    TaskOutcome = "passed"
	TaskFailed    TaskOutcome = "failed"
	TaskErrored   TaskOutcome = "errored"
	TaskBudgetCap TaskOutcome = "budget_cap"
	TaskCancelled TaskOutcome = "cancelled"
)

// TaskResult is a task workflow's result: hashes and IDs, never bytes.
type TaskResult struct {
	Outcome      TaskOutcome
	AttemptID    string
	ArtifactHash string
	Reason       contracts.FailureReason
	BudgetKind   llm.BudgetKind
	Attempts     int
}

// acts names the activities. Its methods are never called here: a method
// value on the nil pointer gives ExecuteActivity the registered name and
// lets the compiler check the arguments.
var acts *activity.Activities
