package main

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/austmsk/conclave/internal/workflow"
)

// runningQuery selects every open execution in the namespace.
const runningQuery = "ExecutionStatus = 'Running'"

// maxListPages bounds how many pages of open workflows are read, at
// listPageSize each: far beyond one owner's work, and a bound on a
// listing that never ends.
const (
	listPageSize = 1000
	maxListPages = 100
)

// temporalRuns is runs over a Temporal client.
type temporalRuns struct {
	c         client.Client
	namespace string
}

func (t temporalRuns) open(ctx context.Context) ([]run, error) {
	var out []run
	var token []byte
	for range maxListPages {
		resp, err := t.c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Namespace: t.namespace, PageSize: listPageSize, NextPageToken: token, Query: runningQuery,
		})
		if err != nil {
			return nil, fmt.Errorf("listing workflows: %w", err)
		}
		for _, e := range resp.GetExecutions() {
			out = append(out, run{
				ID: e.GetExecution().GetWorkflowId(), RunID: e.GetExecution().GetRunId(),
				ParentID: e.GetParentExecution().GetWorkflowId(), Start: e.GetStartTime().AsTime(),
			})
		}
		if token = resp.GetNextPageToken(); len(token) == 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("listing workflows: more than %d pages", maxListPages)
}

func (t temporalRuns) cancel(ctx context.Context, id, runID string) error {
	return ignoreNotFound(t.c.CancelWorkflow(ctx, id, runID))
}

func (t temporalRuns) pauseReaper(ctx context.Context, note string) error {
	h := t.c.ScheduleClient().GetHandle(ctx, workflow.ReaperScheduleID)
	return ignoreNotFound(h.Pause(ctx, client.SchedulePauseOptions{Note: note}))
}

func (t temporalRuns) unpauseReaper(ctx context.Context, note string) error {
	h := t.c.ScheduleClient().GetHandle(ctx, workflow.ReaperScheduleID)
	return ignoreNotFound(h.Unpause(ctx, client.ScheduleUnpauseOptions{Note: note}))
}

// ignoreNotFound treats a run that already closed, or a schedule no worker
// has created yet, as done.
func ignoreNotFound(err error) error {
	var nf *serviceerror.NotFound
	if errors.As(err, &nf) {
		return nil
	}
	return err
}
