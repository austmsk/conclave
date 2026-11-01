package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/austmsk/conclave/internal/workflow"
)

// reapEvery is the default interval of the scheduled reaper.
const reapEvery = 15 * time.Minute

// minReapEvery is the shortest interval accepted: a sweep lists every
// sandbox container, and more often than this buys nothing.
const minReapEvery = time.Minute

// reaperSchedule is the schedule the worker keeps: ReaperWorkflow every
// interval on the workflow queue with the sweep cutoff as its argument,
// skipping a run while the previous one is still going.
func reaperSchedule(every, olderThan time.Duration) (client.ScheduleSpec, *client.ScheduleWorkflowAction) {
	spec := client.ScheduleSpec{Intervals: []client.ScheduleIntervalSpec{{Every: every}}}
	action := &client.ScheduleWorkflowAction{
		ID:        workflow.ReaperWorkflowID,
		Workflow:  workflow.ReaperWorkflow,
		Args:      []any{workflow.ReaperInput{OlderThan: olderThan}},
		TaskQueue: workflow.TaskQueue,
	}
	return spec, action
}

// ensureReaperSchedule creates the reaper's schedule, or updates the
// existing one to this worker's interval and cutoff (NFR-R3). An update
// keeps the schedule's paused state: a halt pauses it, and only
// `conclave resume` unpauses it.
func ensureReaperSchedule(ctx context.Context, sc client.ScheduleClient, every, olderThan time.Duration) error {
	if every < minReapEvery {
		return fmt.Errorf("-reap-every %s is below %s", every, minReapEvery)
	}
	spec, action := reaperSchedule(every, olderThan)
	_, err := sc.Create(ctx, client.ScheduleOptions{
		ID: workflow.ReaperScheduleID, Spec: spec, Action: action, Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, temporal.ErrScheduleAlreadyRunning) {
		return fmt.Errorf("creating the reaper schedule: %w", err)
	}
	err = sc.GetHandle(ctx, workflow.ReaperScheduleID).Update(ctx, client.ScheduleUpdateOptions{
		DoUpdate: func(in client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
			s := in.Description.Schedule
			s.Spec, s.Action = &spec, action
			if s.Policy == nil {
				s.Policy = &client.SchedulePolicies{}
			}
			s.Policy.Overlap = enumspb.SCHEDULE_OVERLAP_POLICY_SKIP
			return &client.ScheduleUpdate{Schedule: &s}, nil
		},
	})
	if err != nil {
		return fmt.Errorf("updating the reaper schedule: %w", err)
	}
	return nil
}
