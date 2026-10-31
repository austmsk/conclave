package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/workflow"
)

// fakeClient answers the two calls the starter makes; every other method
// of client.Client panics through the nil embedded interface.
type fakeClient struct {
	client.Client
	startErr  error
	signalErr error

	opts       client.StartWorkflowOptions
	input      workflow.FeatureInput
	closed     workflow.ClosedCommandInput
	signalName string
	signalArg  github.CommandSignal
	workflowID string
}

func (c *fakeClient) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	c.opts = opts
	switch in := args[0].(type) {
	case workflow.FeatureInput:
		c.input = in
	case workflow.ClosedCommandInput:
		c.closed = in
	}
	return nil, c.startErr
}

func (c *fakeClient) SignalWorkflow(_ context.Context, id, _ string, name string, arg any) error {
	c.workflowID, c.signalName, c.signalArg = id, name, arg.(github.CommandSignal)
	return c.signalErr
}

func newStarter(c *fakeClient) temporalStarter {
	return temporalStarter{client: c, taskQueue: workflow.TaskQueue, ownerID: 4242, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestStartFeature(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr error
	}{
		{name: "started"},
		{name: "duplicate", err: serviceerror.NewWorkflowExecutionAlreadyStarted("started", "", ""), wantErr: github.ErrAlreadyStarted},
		{name: "unavailable", err: serviceerror.NewUnavailable("down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClient{startErr: tt.err}
			req := github.FeatureStart{WorkflowID: github.FeatureWorkflowID(99, 7), DeliveryID: "d1"}
			err := newStarter(c).StartFeature(context.Background(), req)
			switch {
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			case tt.err == nil && err != nil:
				t.Fatalf("error = %v", err)
			case tt.err != nil && err == nil:
				t.Fatal("the engine's error was swallowed")
			}
			if c.opts.ID != req.WorkflowID || c.opts.TaskQueue != workflow.TaskQueue ||
				c.opts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE ||
				!c.opts.WorkflowExecutionErrorWhenAlreadyStarted {
				t.Errorf("options = %+v, want the derived ID, the feature queue and RejectDuplicate", c.opts)
			}
			if c.input.OwnerUserID != 4242 || c.input.Start.WorkflowID != req.WorkflowID {
				t.Errorf("input = %+v", c.input)
			}
		})
	}
}

func TestSignalCommand(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr error
	}{
		{name: "delivered"},
		{name: "no workflow", err: serviceerror.NewNotFound("gone"), wantErr: github.ErrNoWorkflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClient{signalErr: tt.err}
			req := github.CommandSignal{WorkflowID: "feature/99/7", CommentID: 5, Command: github.Command{Name: github.CommandApprove}}
			err := newStarter(c).SignalCommand(context.Background(), req)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if c.workflowID != req.WorkflowID || c.signalName != "approve" || c.signalArg.CommentID != 5 {
				t.Errorf("signalled %s %q %+v", c.workflowID, c.signalName, c.signalArg)
			}
		})
	}
}

// TestStartClosedCommand starts the closed-run workflow under the ID
// derived from the comment, with RejectDuplicate, and maps a duplicate
// start to ErrAlreadyStarted, so a redelivered comment is answered once.
func TestStartClosedCommand(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr error
	}{
		{name: "started"},
		{name: "duplicate", err: serviceerror.NewWorkflowExecutionAlreadyStarted("started", "", ""), wantErr: github.ErrAlreadyStarted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClient{startErr: tt.err}
			req := github.CommandSignal{
				WorkflowID: github.FeatureWorkflowID(99, 7), Repo: github.RepoRef{ID: 99}, IssueNumber: 7, CommentID: 5,
				ActorID: 4242, Command: github.Command{Name: github.CommandRetry},
			}
			err := newStarter(c).StartClosedCommand(context.Background(), req)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if c.opts.ID != "command/99/7/5" || c.opts.TaskQueue != workflow.TaskQueue ||
				c.opts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE {
				t.Errorf("options = %+v, want the comment's ID, the feature queue and RejectDuplicate", c.opts)
			}
			if c.closed.OwnerUserID != 4242 || c.closed.Signal.CommentID != 5 {
				t.Errorf("input = %+v", c.closed)
			}
		})
	}
}

// liveTemporalEnv names the Temporal frontend for the live starter test,
// such as localhost:7233 from make dev. Unset, the test skips: CI has no
// Temporal server, and the fake above covers the error mapping.
const liveTemporalEnv = "CONCLAVE_TEMPORAL_ADDRESS"

// TestStarterAgainstLiveTemporal has a real server refuse a second start
// of an issue's workflow ID, while the first runs and after it has closed
// (RejectDuplicate), and report a signal to an ID with no workflow as
// ErrNoWorkflow. Nothing polls the test's task queue, so no workflow
// code runs.
func TestStarterAgainstLiveTemporal(t *testing.T) {
	addr := os.Getenv(liveTemporalEnv)
	if addr == "" {
		t.Skipf("%s is not set", liveTemporalEnv)
	}
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("dialling %s: %v", addr, err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := temporalStarter{client: c, taskQueue: "t8b-starter-test", ownerID: 4242, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := github.FeatureStart{WorkflowID: fmt.Sprintf("t8b-start-%d", time.Now().UnixNano()), DeliveryID: "d1"}
	if err := s.StartFeature(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := s.StartFeature(ctx, req); !errors.Is(err, github.ErrAlreadyStarted) {
		t.Fatalf("second start while running: %v, want ErrAlreadyStarted", err)
	}
	if err := c.TerminateWorkflow(ctx, req.WorkflowID, "", "test cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := s.StartFeature(ctx, req); !errors.Is(err, github.ErrAlreadyStarted) {
		t.Fatalf("start after the run closed: %v, want ErrAlreadyStarted", err)
	}
	none := github.CommandSignal{WorkflowID: req.WorkflowID + "-none", Command: github.Command{Name: github.CommandCancel}}
	if err := s.SignalCommand(ctx, none); !errors.Is(err, github.ErrNoWorkflow) {
		t.Fatalf("signal to no workflow: %v, want ErrNoWorkflow", err)
	}
}
