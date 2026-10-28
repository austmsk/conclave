package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

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
	signalName string
	signalArg  github.CommandSignal
	workflowID string
}

func (c *fakeClient) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	c.opts = opts
	c.input = args[0].(workflow.FeatureInput)
	return nil, c.startErr
}

func (c *fakeClient) SignalWorkflow(_ context.Context, id, _ string, name string, arg any) error {
	c.workflowID, c.signalName, c.signalArg = id, name, arg.(github.CommandSignal)
	return c.signalErr
}

func newStarter(c *fakeClient) temporalStarter {
	return temporalStarter{client: c, ownerID: 4242, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
