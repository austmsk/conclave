package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/alert"
	"github.com/austmsk/conclave/internal/store"
	"github.com/austmsk/conclave/internal/testenv"
	cwf "github.com/austmsk/conclave/internal/workflow"
)

const liveQueue = "halt-live"

// releases records which runs' deferred cleanup ran: the stand-in for a
// sandbox release, so the test can see that cancelling a parent reached
// its child and that both cleaned up.
type releases struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (r *releases) Release(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids[id] = true
	return nil
}

func (r *releases) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ids)
}

// release runs the cleanup activity on a disconnected context, as the
// real workflows release their sandboxes (RL-3).
func release(ctx workflow.Context) {
	dctx, _ := workflow.NewDisconnectedContext(ctx)
	dctx = workflow.WithActivityOptions(dctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	_ = workflow.ExecuteActivity(dctx, "Release", workflow.GetInfo(ctx).WorkflowExecution.ID).Get(dctx, nil)
}

// liveParent stands in for FeatureWorkflow: it runs a child, as the
// feature workflow runs TaskWorkflow, and waits on it until cancelled.
func liveParent(ctx workflow.Context) error {
	defer release(ctx)
	cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID + "/child",
	})
	return workflow.ExecuteChildWorkflow(cctx, liveChild).Get(ctx, nil)
}

// liveChild waits until cancelled, as an attempt in progress does.
func liveChild(ctx workflow.Context) error {
	defer release(ctx)
	return workflow.Await(ctx, func() bool { return false })
}

func discardLogger() tlog.Logger {
	return tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestHaltAgainstLiveTemporal is the kill switch against a real Temporal
// server (FR-SEC11): two runs with children are open, and a third starts
// while the halt is draining. halt pauses the reaper's schedule, cancels
// every root, and finishes only when nothing is open, with every run's
// deferred cleanup done; children are reached through their parents.
// A run started while halted is then cancelled by resume, which unpauses
// the schedule.
func TestHaltAgainstLiveTemporal(t *testing.T) {
	addr := testenv.StartTemporal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := client.Dial(client.Options{HostPort: addr, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last first: the worker stops before its client closes.
	t.Cleanup(c.Close)
	rel := startLiveWorker(t, c)

	start := func(id string) {
		t.Helper()
		if _, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: liveQueue}, liveParent); err != nil {
			t.Fatal(err)
		}
	}
	createReaperSchedule(ctx, t, c)
	start("feature/1/1")
	start("feature/1/2")
	k := newLiveKillSwitch(c)
	waitOpen(ctx, t, k.runs, 4)

	// The late start arrives while the halt drains: the halt alert is
	// raised just before the first listing.
	raise := k.raise
	k.raise = func(ctx context.Context, a alert.Alert) error {
		if a.Severity == alert.SeverityHalt && a.Title == "Kill switch: all work halted" {
			start("feature/1/3")
		}
		return raise(ctx, a)
	}
	if err := k.halt(ctx, "live drill"); err != nil {
		t.Fatalf("halt: %v\n%s", err, k.out)
	}
	if open, err := k.runs.open(ctx); err != nil || len(open) != 0 {
		t.Fatalf("after halt: %d open (%v), want none", len(open), err)
	}
	checkCancelled(ctx, t, c, "feature/1/1", "feature/1/2", "feature/1/3")
	if n := rel.count(); n != 6 {
		t.Errorf("%d runs cleaned up, want 6 (three parents, three children)", n)
	}
	if !schedulePaused(ctx, t, c) {
		t.Error("the reaper's schedule is not paused")
	}

	// A label while halted starts a run no worker would see until resume.
	time.Sleep(time.Second)
	start("feature/1/4")
	if err := k.resume(ctx, "drill over", false); err != nil {
		t.Fatal(err)
	}
	waitOpen(ctx, t, k.runs, 0)
	if schedulePaused(ctx, t, c) {
		t.Error("the reaper's schedule is still paused after resume")
	}
	if st, _ := k.currentHalt(ctx); st.Halted {
		t.Error("still halted after resume")
	}
}

// startLiveWorker runs the stand-in workflows on liveQueue until the test
// ends, and returns the record of their cleanups.
func startLiveWorker(t *testing.T, c client.Client) *releases {
	t.Helper()
	rel := &releases{ids: map[string]bool{}}
	w := worker.New(c, liveQueue, worker.Options{})
	w.RegisterWorkflow(liveParent)
	w.RegisterWorkflow(liveChild)
	w.RegisterActivity(rel)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	return rel
}

// createReaperSchedule stands in for the worker's schedule, hourly, so
// halt has a schedule to pause.
func createReaperSchedule(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	if _, err := c.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID:     cwf.ReaperScheduleID,
		Spec:   client.ScheduleSpec{Intervals: []client.ScheduleIntervalSpec{{Every: time.Hour}}},
		Action: &client.ScheduleWorkflowAction{ID: "reaper", Workflow: liveChild, TaskQueue: liveQueue},
	}); err != nil {
		t.Fatal(err)
	}
}

// checkCancelled requires each workflow to have ended cancelled.
func checkCancelled(ctx context.Context, t *testing.T, c client.Client, ids ...string) {
	t.Helper()
	for _, id := range ids {
		desc, err := c.DescribeWorkflowExecution(ctx, id, "")
		if err != nil {
			t.Fatal(err)
		}
		if s := desc.GetWorkflowExecutionInfo().GetStatus(); s != enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED {
			t.Errorf("%s ended %s, want cancelled", id, s)
		}
	}
}

// newLiveKillSwitch is the kill switch over the live server, with the
// halt state, the sweep and the alerts in memory.
func newLiveKillSwitch(c client.Client) *killSwitch {
	var (
		mu sync.Mutex
		st store.HaltState
	)
	set := func(halted bool) func(context.Context, string) (store.HaltState, error) {
		return func(_ context.Context, reason string) (store.HaltState, error) {
			mu.Lock()
			defer mu.Unlock()
			if st.Halted != halted {
				st = store.HaltState{Halted: halted, ID: st.ID + 1, Since: time.Now(), Reason: reason}
			}
			return st, nil
		}
	}
	return &killSwitch{
		setHalt: set(true), clearHalt: set(false),
		currentHalt: func(context.Context) (store.HaltState, error) { mu.Lock(); defer mu.Unlock(); return st, nil },
		runs:        temporalRuns{c: c, namespace: "default"},
		sweep:       func(context.Context) (activity.SweepResult, error) { return activity.SweepResult{}, nil },
		remaining:   func(context.Context) (int, int, error) { return 0, 0, nil },
		raise:       func(context.Context, alert.Alert) error { return nil },
		drain:       2 * time.Minute, poll: 500 * time.Millisecond, now: time.Now, sleep: sleepCtx,
		out: &bytes.Buffer{},
	}
}

// waitOpen waits until the namespace lists n open runs. Visibility trails
// the start and the close by a moment.
func waitOpen(ctx context.Context, t *testing.T, r runs, n int) {
	t.Helper()
	for {
		open, err := r.open(ctx)
		if err == nil && len(open) == n {
			return
		}
		if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
			t.Fatalf("waiting for %d open runs: last saw %d (%v)", n, len(open), err)
		}
	}
}

func schedulePaused(ctx context.Context, t *testing.T, c client.Client) bool {
	t.Helper()
	d, err := c.ScheduleClient().GetHandle(ctx, cwf.ReaperScheduleID).Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d.Schedule.State.Paused
}
