package workflow

import (
	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
)

// Task queues. Workflows and the small activities share TaskQueue;
// RunAttempt runs on the attempt queue beside it, which the worker polls
// with one slot, so two issues labelled at once queue their attempts. A
// workflow on any queue sends its attempts to that queue's attempt queue,
// so a test or a benchmark on its own queue never reaches another
// worker's.
const (
	TaskQueue          = "conclave"
	attemptQueueSuffix = "-attempt"
	AttemptTaskQueue   = TaskQueue + attemptQueueSuffix
)

// AttemptQueue is the attempt queue that pairs with a workflow task queue.
func AttemptQueue(taskQueue string) string {
	return taskQueue + attemptQueueSuffix
}

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
// order. validCommands decides which of them apply in each status.
var commandNames = []github.CommandName{
	github.CommandApprove, github.CommandCancel, github.CommandRevise, github.CommandAsk,
	github.CommandStatus, github.CommandRetry, github.CommandHelp,
}

// Status is the feature workflow's view of its feature request, returned
// by QueryStatus. PhaseLeft is the status the feature request left for
// needs_attention: /retry returns to it. Reason and PhaseLeft are cleared
// on every move out of needs_attention. PhaseLeft lives here, not in
// Postgres; state_transitions.from_state records it durably.
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
// is GitHub state: the repository is named by full name, and the
// attempt's tree source reads the rest from the plan body, so Milestone
// 2's benchmark starts the workflow directly, with no GitHub App.
type TaskInput struct {
	TaskID       string
	Ordinal      int
	BaseSHA      string
	RepoFullName string
	Role         contracts.Role
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
	// Detail says what went wrong when Outcome is errored for a reason
	// other than the attempt's own: a permanent refusal, a failed step.
	Detail   string
	Attempts int
}

// acts names the activities. Its methods are never called here: a method
// value on the nil pointer gives ExecuteActivity the registered name and
// lets the compiler check the arguments.
var acts *activity.Activities
