package github

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"
)

// WorkflowStarter is how the receiver hands work to the workflow engine.
// Ticket 8 implements it on Temporal; until then cmd/server logs instead.
type WorkflowStarter interface {
	// StartFeature starts the feature workflow for an issue. A start
	// whose WorkflowID is already in use returns ErrAlreadyStarted.
	StartFeature(ctx context.Context, req FeatureStart) error
	// SignalCommand delivers an owner command to a running feature
	// workflow. It returns ErrNoWorkflow when none is running under
	// WorkflowID.
	SignalCommand(ctx context.Context, req CommandSignal) error
}

var (
	// ErrAlreadyStarted reports a start for a workflow ID already in use:
	// a redelivered or replayed label event. It is the expected outcome of
	// a duplicate, not a failure.
	ErrAlreadyStarted = errors.New("workflow already started")
	// ErrNoWorkflow reports a command for an issue with no running
	// workflow.
	ErrNoWorkflow = errors.New("no running workflow")
)

// RepoRef identifies a repository by its numeric ID, which survives a
// rename, with its current full name for URLs and logs.
type RepoRef struct {
	ID             int64
	FullName       string
	InstallationID int64
}

// IssueSnapshot is the issue as the signed labeled event carried it. The
// run works from this snapshot, so an author who edits the issue after the
// owner labels it cannot change what the owner approved for work.
type IssueSnapshot struct {
	Number int
	Title  string
	Body   string
	// Author is the issue's author, which need not be the owner: the owner
	// may label a stranger's issue. Its Source decides how the body is
	// quarantined.
	Author Author
}

// FeatureStart asks for a run on an issue.
type FeatureStart struct {
	WorkflowID string
	DeliveryID string
	Repo       RepoRef
	Issue      IssueSnapshot
	// LabelerID is the account that applied the trigger label: always the
	// owner, recorded for the audit trail.
	LabelerID int64
}

// CommandSignal carries an owner command to an issue's workflow.
type CommandSignal struct {
	WorkflowID  string
	DeliveryID  string
	Repo        RepoRef
	IssueNumber int
	// CommentID identifies the command. A redelivered or replayed delivery
	// carries the same ID, so the workflow can ignore a command it has
	// already applied.
	CommentID int64
	ActorID   int64
	Command   Command
}

// FeatureWorkflowID is the workflow ID for an issue's run. It derives from
// the repository's numeric ID and the issue number alone, so every
// delivery about the same issue, duplicated, redelivered, or sent after a
// rename, names the same workflow, and the engine refuses a second start.
func FeatureWorkflowID(repoID int64, issue int) string {
	return "feature/" + strconv.FormatInt(repoID, 10) + "/" + strconv.Itoa(issue)
}

// job is one unit of dispatch: exactly one of start and signal is set.
type job struct {
	start  *FeatureStart
	signal *CommandSignal
}

// Dispatcher runs starter calls off the request path, one at a time and in
// arrival order, so a label is handled before the /approve that follows
// it. The receiver enqueues without blocking; a full queue is refused.
//
// L0 stub: W1 implements.
type Dispatcher struct {
	starter WorkflowStarter
	log     *slog.Logger
	jobs    chan job
}

// NewDispatcher returns a dispatcher with a queue of depth jobs.
//
// L0 stub: W1 implements.
func NewDispatcher(s WorkflowStarter, log *slog.Logger, depth int) *Dispatcher {
	return &Dispatcher{starter: s, log: log, jobs: make(chan job, depth)}
}

// Run consumes the queue until Close drains it or ctx ends.
//
// L0 stub: W1 implements.
func (d *Dispatcher) Run(ctx context.Context) { <-ctx.Done() }

// Close stops accepting jobs and waits up to drain for queued jobs to be
// dispatched. Jobs still queued after that are logged by delivery ID, so
// the owner can redeliver them.
//
// L0 stub: W1 implements.
func (d *Dispatcher) Close(drain time.Duration) { _ = drain }

// enqueue adds j without blocking and reports whether it was accepted.
//
// L0 stub: W1 implements.
func (d *Dispatcher) enqueue(j job) bool {
	select {
	case d.jobs <- j:
		return true
	default:
		return false
	}
}
