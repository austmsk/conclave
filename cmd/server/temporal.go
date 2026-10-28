package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/workflow"
)

// temporalStarter is the receiver's workflow engine: it starts one
// feature workflow per issue and delivers owner commands as signals.
type temporalStarter struct {
	client  client.Client
	ownerID int64
	log     *slog.Logger
}

var _ github.WorkflowStarter = temporalStarter{}

// StartFeature starts the issue's feature workflow under the ID derived
// from the issue, refusing a duplicate: a redelivered label event, or a
// relabel of an issue whose run already closed, is ErrAlreadyStarted, the
// expected outcome of a duplicate (Milestone 1 does not rerun an issue).
func (s temporalStarter) StartFeature(ctx context.Context, req github.FeatureStart) error {
	_, err := s.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       req.WorkflowID,
		TaskQueue:                workflow.TaskQueue,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, workflow.FeatureWorkflow, workflow.FeatureInput{Start: req, OwnerUserID: s.ownerID})
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &started) {
		s.log.Info("feature workflow already started; duplicate start refused",
			"workflow_id", req.WorkflowID, "delivery", req.DeliveryID)
		return fmt.Errorf("starting %s: %w", req.WorkflowID, github.ErrAlreadyStarted)
	}
	if err != nil {
		return fmt.Errorf("starting %s: %w", req.WorkflowID, err)
	}
	return nil
}

// SignalCommand signals the running feature workflow on the command's
// own signal name. The workflow drops what does not apply and ignores a
// comment ID it has seen.
func (s temporalStarter) SignalCommand(ctx context.Context, req github.CommandSignal) error {
	err := s.client.SignalWorkflow(ctx, req.WorkflowID, "", workflow.SignalName(req.Command.Name), req)
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return fmt.Errorf("signalling %s: %w", req.WorkflowID, github.ErrNoWorkflow)
	}
	if err != nil {
		return fmt.Errorf("signalling %s: %w", req.WorkflowID, err)
	}
	return nil
}
