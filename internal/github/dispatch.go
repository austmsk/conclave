package github

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
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

// ids returns the job's workflow and delivery IDs for the log.
func (j job) ids() (workflowID, deliveryID string) {
	if j.start != nil {
		return j.start.WorkflowID, j.start.DeliveryID
	}
	return j.signal.WorkflowID, j.signal.DeliveryID
}

// callTimeout bounds one starter call, so an engine that stops answering
// holds the queue for at most this long per job.
const callTimeout = 10 * time.Second

// Dispatcher runs starter calls off the request path, one at a time and in
// arrival order, so a label is handled before the /approve that follows
// it. The receiver enqueues without blocking; a full queue is refused.
//
// A failed call is logged and not retried. The engine dedupes by workflow
// ID and comment ID, so the owner's remedy, redelivering from GitHub's
// delivery log, is always safe; a retry loop here would only hide an
// outage behind a queue that has stopped moving.
type Dispatcher struct {
	starter WorkflowStarter
	log     *slog.Logger
	jobs    chan job

	// mu guards closed, so enqueue never sends on the closed jobs channel.
	mu     sync.Mutex
	closed bool

	// abort ends Run, and the call it is making, once Close's drain
	// deadline passes.
	abort    context.Context
	abortRun context.CancelFunc
	// done is closed when Run returns.
	done     chan struct{}
	doneOnce sync.Once
}

// NewDispatcher returns a dispatcher with a queue of depth jobs.
func NewDispatcher(s WorkflowStarter, log *slog.Logger, depth int) *Dispatcher {
	abort, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		starter:  s,
		log:      log,
		jobs:     make(chan job, depth),
		abort:    abort,
		abortRun: cancel,
		done:     make(chan struct{}),
	}
}

// Run consumes the queue until Close drains it or ctx ends. It is called
// once, from its own goroutine. Each starter call's timeout derives from
// ctx, so ctx should outlive shutdown: Close, not ctx, is how a dispatcher
// is meant to stop, because a cancelled ctx abandons whatever is queued.
func (d *Dispatcher) Run(ctx context.Context) {
	defer d.doneOnce.Do(func() { close(d.done) })
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(d.abort, cancel)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-d.jobs:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				d.dropped(j)
				return
			}
			d.dispatch(ctx, j)
		}
	}
}

// dispatch makes one starter call and logs its outcome. A duplicate start
// and a command for an issue with no run are expected outcomes, not
// failures.
func (d *Dispatcher) dispatch(ctx context.Context, j job) {
	err := d.call(ctx, j)
	workflowID, deliveryID := j.ids()
	outcome := "dispatched"
	switch {
	case err == nil:
	case errors.Is(err, ErrAlreadyStarted):
		outcome = "duplicate"
	case errors.Is(err, ErrNoWorkflow):
		outcome = "no_workflow"
	default:
		d.log.Error("webhook dispatch failed; redeliver to retry",
			"workflow_id", workflowID, "delivery", deliveryID, "error", err)
		return
	}
	d.log.Info("webhook dispatched", "outcome", outcome, "workflow_id", workflowID, "delivery", deliveryID)
}

// errStarterPanicked stands for a starter call that panicked. The panic
// value is not kept: it is the engine client's text, and could hold
// anything.
var errStarterPanicked = errors.New("starter panicked")

// call makes one starter call. A panic in it is recovered and becomes
// errStarterPanicked: this goroutine is the server's only consumer, and a
// panic left to unwind would take the webhook endpoint down with it, where
// one on the request path costs only that delivery.
func (d *Dispatcher) call(ctx context.Context, j job) (err error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			err = errStarterPanicked
		}
	}()
	if j.start != nil {
		return d.starter.StartFeature(ctx, *j.start)
	}
	return d.starter.SignalCommand(ctx, *j.signal)
}

// Close stops accepting jobs and waits up to drain for queued jobs to be
// dispatched. Jobs still queued after that are logged by delivery ID, so
// the owner can redeliver them. It is safe to call more than once.
func (d *Dispatcher) Close(drain time.Duration) {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.jobs)
	}
	d.mu.Unlock()

	timer := time.NewTimer(drain)
	defer timer.Stop()
	select {
	case <-d.done:
	case <-timer.C:
		d.abortRun()
	}
	// Whatever is left was never dispatched: Run ended on its context, or
	// the deadline passed. Run may still take one job after the abort; it
	// logs that one itself.
	for j := range d.jobs {
		d.dropped(j)
	}
}

func (d *Dispatcher) dropped(j job) {
	workflowID, deliveryID := j.ids()
	d.log.Warn("webhook dropped undispatched at shutdown; redeliver to retry",
		"workflow_id", workflowID, "delivery", deliveryID)
}

// enqueue adds j without blocking and reports whether it was accepted. It
// refuses once Close has been called.
func (d *Dispatcher) enqueue(j job) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	select {
	case d.jobs <- j:
		return true
	default:
		return false
	}
}
