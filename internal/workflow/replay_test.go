package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/temporalproto"
	sdkactivity "go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
)

// The captured histories: the feature workflow waiting on its child, and
// the child with its attempt activity scheduled and not yet completed.
const (
	featureHistory = "testdata/feature-mid-attempt.json"
	taskHistory    = "testdata/task-mid-attempt.json"
)

// captureEnv gates TestCaptureReplayHistory, which needs a running
// Temporal (make dev). It rewrites the files above.
const captureEnv = "CONCLAVE_CAPTURE_REPLAY"

// TestReplayMidAttempt replays histories captured from a real Temporal
// server while the attempt was running, offline. The replayer re-executes
// the workflow code against the recorded events and fails on any command
// that disagrees with them, so a pass means a worker resuming this run
// would schedule nothing already done: no completed activity is run again
// (NFR-R1). The real two-worker kill test is ticket 10's.
func TestReplayMidAttempt(t *testing.T) {
	tests := []struct {
		file          string
		wantCompleted []string
		wantPending   string
	}{
		{featureHistory, []string{"Analyze", "Transition", "PostPlanComment", "Approve", "CreateTask"}, ""},
		{taskHistory, []string{"PrepareAttempt"}, "RunAttempt"},
	}
	for _, tt := range tests {
		t.Run(filepath.Base(tt.file), func(t *testing.T) {
			hist, err := client.HistoryFromJSON(mustOpen(t, tt.file), client.HistoryJSONOptions{})
			if err != nil {
				t.Fatal(err)
			}
			completed, pending := activityProgress(hist)
			for _, name := range tt.wantCompleted {
				if completed[name] == 0 {
					t.Errorf("history has no completed %s; recapture it", name)
				}
			}
			if tt.wantPending != "" && pending[tt.wantPending] == 0 {
				t.Errorf("history is not mid-attempt: %s is not pending", tt.wantPending)
			}
			r := worker.NewWorkflowReplayer()
			r.RegisterWorkflow(FeatureWorkflow)
			r.RegisterWorkflow(TaskWorkflow)
			if err := r.ReplayWorkflowHistory(nil, hist); err != nil {
				t.Fatalf("replay: %v", err)
			}
		})
	}
}

// activityProgress counts, by activity name, the activities a history
// completed and the ones it scheduled and has not finished.
func activityProgress(h *historypb.History) (completed, pending map[string]int) {
	completed, pending = map[string]int{}, map[string]int{}
	names := map[int64]string{}
	for _, e := range h.GetEvents() {
		switch e.GetEventType() {
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			name := e.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName()
			names[e.GetEventId()] = name
			pending[name]++
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			name := names[e.GetActivityTaskCompletedEventAttributes().GetScheduledEventId()]
			pending[name]--
			completed[name]++
		}
	}
	return completed, pending
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestCaptureReplayHistory records the replay fixtures from the shared
// dev Temporal at localhost:7233, on this branch's own task queue and
// workflow ID prefix. The activities are the test's fakes; the attempt
// blocks until both histories are fetched, so they end mid-attempt. Run
// it with CONCLAVE_CAPTURE_REPLAY=1 after any change that the replay test
// reports as nondeterministic, and review the diff.
func TestCaptureReplayHistory(t *testing.T) {
	if os.Getenv(captureEnv) == "" {
		t.Skipf("set %s=1 with Temporal running to recapture the replay fixtures", captureEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := client.Dial(client.Options{})
	if err != nil {
		t.Fatalf("dialling Temporal: %v", err)
	}
	defer c.Close()
	const queue = "t8b-replay"
	started, release := make(chan struct{}), make(chan struct{})
	w := newWorld()
	var once sync.Once
	w.run = func(ctx context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return w.finish(n, contracts.AttemptPassed, ""), nil
	}
	stop := startCaptureWorkers(t, c, queue, w)
	defer stop()

	in := featureInput()
	in.Start.WorkflowID = fmt.Sprintf("t8b-feature-%d", time.Now().UnixNano())
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.Start.WorkflowID, TaskQueue: queue}, FeatureWorkflow, in)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.TerminateWorkflow(context.Background(), run.GetID(), "", "replay capture done") }()
	waitForStatus(ctx, t, c, run.GetID(), contracts.FeatureAwaitingApproval)
	if err := c.SignalWorkflow(ctx, run.GetID(), "", SignalName(github.CommandApprove), command(github.CommandApprove, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("the attempt never started")
	}
	writeHistory(ctx, t, c, run.GetID(), featureHistory)
	writeHistory(ctx, t, c, run.GetID()+"/task/"+taskID, taskHistory)
	close(release)
}

// startCaptureWorkers runs the workflows and w's activities on queue and
// its attempt queue.
func startCaptureWorkers(t *testing.T, c client.Client, queue string, w *world) func() {
	t.Helper()
	main := worker.New(c, queue, worker.Options{})
	main.RegisterWorkflow(FeatureWorkflow)
	main.RegisterWorkflow(TaskWorkflow)
	w.registerOn(main)
	attempts := worker.New(c, AttemptQueue(queue), worker.Options{MaxConcurrentActivityExecutionSize: 1})
	attempts.RegisterActivityWithOptions(w.RunAttempt, sdkactivity.RegisterOptions{Name: "RunAttempt"})
	for _, wk := range []worker.Worker{main, attempts} {
		if err := wk.Start(); err != nil {
			t.Fatal(err)
		}
	}
	return func() { main.Stop(); attempts.Stop() }
}

func waitForStatus(ctx context.Context, t *testing.T, c client.Client, id string, want contracts.FeatureRequestStatus) {
	t.Helper()
	for ctx.Err() == nil {
		v, err := c.QueryWorkflow(ctx, id, "", QueryStatus)
		if err == nil {
			var st Status
			if v.Get(&st) == nil && st.Status == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("workflow %s never reached %s", id, want)
}

func writeHistory(ctx context.Context, t *testing.T, c client.Client, id, path string) {
	t.Helper()
	it := c.GetWorkflowHistory(ctx, id, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	h := &historypb.History{}
	for it.HasNext() {
		e, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		h.Events = append(h.Events, e)
	}
	data, err := temporalproto.CustomJSONMarshalOptions{Indent: "  "}.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
