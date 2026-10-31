package githubtest

import (
	"context"
	"sync"

	"github.com/austmsk/conclave/internal/github"
)

// Call is one call a FakeStarter answered: exactly one of Start, Signal
// and Closed is set, and Err is what the call returned. Closed is a
// StartClosedCommand, and WorkflowID the ID it started.
type Call struct {
	Start      *github.FeatureStart
	Signal     *github.CommandSignal
	Closed     *github.CommandSignal
	WorkflowID string
	Err        error
}

// FakeStarter is a github.WorkflowStarter that behaves like the engine on
// the cases the receiver depends on: a second start under a workflow ID
// already started returns github.ErrAlreadyStarted, as Temporal's
// REJECT_DUPLICATE policy does, and a command for a workflow never started
// returns github.ErrNoWorkflow. It is safe for concurrent use.
//
// Set Block and Err before the first call.
type FakeStarter struct {
	// Block, when set, makes every call wait until it is closed or the
	// call's context ends. A call whose context ends while waiting returns
	// the context's error and is not recorded.
	Block chan struct{}
	// Err, when set, is returned by every call, which is still recorded.
	Err error

	mu      sync.Mutex
	started map[string]bool
	calls   []Call
}

var _ github.WorkflowStarter = (*FakeStarter)(nil)

// Running marks workflow IDs as already running, so commands to them are
// delivered without a start first.
func (f *FakeStarter) Running(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.mark(id)
	}
}

// StartFeature records req and starts its workflow ID.
func (f *FakeStarter) StartFeature(ctx context.Context, req github.FeatureStart) error {
	if err := f.wait(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.Err
	switch {
	case err != nil:
	case f.started[req.WorkflowID]:
		err = github.ErrAlreadyStarted
	default:
		f.mark(req.WorkflowID)
	}
	f.calls = append(f.calls, Call{Start: &req, Err: err})
	return err
}

// SignalCommand records req and reports whether its workflow is running.
func (f *FakeStarter) SignalCommand(ctx context.Context, req github.CommandSignal) error {
	if err := f.wait(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.Err
	if err == nil && !f.started[req.WorkflowID] {
		err = github.ErrNoWorkflow
	}
	f.calls = append(f.calls, Call{Signal: &req, Err: err})
	return err
}

// StartClosedCommand records req and starts its closed-command workflow
// ID, refusing a second start under the same ID as REJECT_DUPLICATE does.
func (f *FakeStarter) StartClosedCommand(ctx context.Context, req github.CommandSignal) error {
	if err := f.wait(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := github.ClosedCommandWorkflowID(req.Repo.ID, req.IssueNumber, req.CommentID)
	err := f.Err
	switch {
	case err != nil:
	case f.started[id]:
		err = github.ErrAlreadyStarted
	default:
		f.mark(id)
	}
	f.calls = append(f.calls, Call{Closed: &req, WorkflowID: id, Err: err})
	return err
}

// Calls returns every recorded call in the order it was answered.
func (f *FakeStarter) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

func (f *FakeStarter) wait(ctx context.Context) error {
	if f.Block == nil {
		return nil
	}
	select {
	case <-f.Block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// mark records id as started. The caller holds f.mu.
func (f *FakeStarter) mark(id string) {
	if f.started == nil {
		f.started = make(map[string]bool)
	}
	f.started[id] = true
}
