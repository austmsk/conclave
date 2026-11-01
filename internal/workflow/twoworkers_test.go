package workflow

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	sdkactivity "go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/testenv"
)

// TestWorkerKilledMidAttemptResumesOnSecond is NFR-R1 against a real
// Temporal server, with two workers: worker A dies while its attempt is
// running, and worker B carries the run to delivered without repeating
// anything that had completed.
//
// The kill is a process death, not a shutdown: A's client connection is
// closed first, so nothing A does afterwards reaches the server, and only
// then are its workers stopped. The server learns of the death the way it
// would in production, when the attempt's heartbeats stop for
// activity.HeartbeatTimeout (two minutes, waited out in real time).
//
// RunAttempt has MaximumAttempts 1 (decision 0012): the dead attempt is
// not resumed or retried, because its artifact hash is write-once and a
// second run would repeat its spend. It settles errored, and the task
// continues with a fresh ordinal on B. What "resumes" means here is the
// workflow: every activity that completed before the kill ran exactly
// once.
func TestWorkerKilledMidAttemptResumesOnSecond(t *testing.T) {
	addr := testenv.StartTemporal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	queue := fmt.Sprintf("t10a-two-workers-%d", time.Now().UnixNano())

	w := newWorld()
	w.run = func(_ context.Context, w *world, n int) (activity.AttemptOutcome, error) {
		return w.finish(n, contracts.AttemptPassed, ""), nil
	}
	started := make(chan struct{})
	a := startDoomedWorkers(t, addr, queue, w, started)
	b := dialWorker(t, addr)
	bMain := mainWorker(b, queue, w)
	startWorkers(t, bMain)
	defer bMain.Stop()

	in := featureInput()
	in.Start.WorkflowID = queue + "-feature"
	run, err := b.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.Start.WorkflowID, TaskQueue: queue}, FeatureWorkflow, in)
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(ctx, t, b, run.GetID(), contracts.FeatureAwaitingApproval)
	if err := b.SignalWorkflow(ctx, run.GetID(), "", SignalName(github.CommandApprove), command(github.CommandApprove, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("the first attempt never started on worker A")
	}
	before := callCounts(w, "Analyze", "PostPlanComment", "Approve", "CreateTask", "PrepareAttempt")
	a.kill()

	// B's attempt worker starts only now, so the first attempt could
	// only have run on A.
	bAttempts := attemptWorker(b, queue, w.RunAttempt, w)
	startWorkers(t, bAttempts)
	defer bAttempts.Stop()
	if err := run.Get(ctx, nil); err != nil {
		t.Fatalf("workflow did not complete on worker B: %v", err)
	}

	equal(t, "feature request", w.fr, contracts.FeatureDelivered)
	equalSlices(t, "attempts", w.attemptStates(), []string{"errored/" + string(contracts.ReasonSandboxError), "passed/"})
	equalSlices(t, "unreleased sandboxes", w.unreleased(), nil)
	equal(t, "attempts on A", a.ran(), 1)
	equal(t, "RunAttempt calls", w.called("RunAttempt"), 2)
	for name, n := range before {
		want := n
		if name == "PrepareAttempt" {
			want++ // ordinal 2, prepared on B
		}
		equal(t, name+" calls", w.called(name), want)
		if n != 1 {
			t.Errorf("%s ran %d times before the kill, want once", name, n)
		}
	}
}

// doomed is worker A: its own client, so the kill can cut it off.
type doomed struct {
	t       *testing.T
	c       client.Client
	workers []worker.Worker
	mu      sync.Mutex
	runs    int
	killed  bool
}

// startDoomedWorkers starts worker A on queue: the workflows and w's
// activities, and an attempt worker whose RunAttempt records the attempt
// as the world does, closes started, and heartbeats until A is killed.
func startDoomedWorkers(t *testing.T, addr, queue string, w *world, started chan struct{}) *doomed {
	t.Helper()
	// A's client is closed by kill, not by test cleanup.
	c, err := client.Dial(client.Options{HostPort: addr, Logger: discardSDKLogger()})
	if err != nil {
		t.Fatalf("dialling Temporal at %s: %v", addr, err)
	}
	d := &doomed{t: t, c: c}
	var once sync.Once
	runAttempt := func(ctx context.Context, in activity.RunAttemptInput) (activity.AttemptOutcome, error) {
		if err := w.count("RunAttempt"); err != nil {
			return activity.AttemptOutcome{}, err
		}
		d.mu.Lock()
		d.runs++
		d.mu.Unlock()
		w.createSandbox(ordinalOf(in.AttemptID))
		once.Do(func() { close(started) })
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return activity.AttemptOutcome{}, ctx.Err()
			case <-tick.C:
				sdkactivity.RecordHeartbeat(ctx)
			}
		}
	}
	d.workers = []worker.Worker{mainWorker(d.c, queue, w), attemptWorker(d.c, queue, runAttempt, w)}
	startWorkers(t, d.workers...)
	t.Cleanup(d.kill)
	return d
}

// kill cuts A off from the server, then stops its workers, which ends the
// blocked attempt; its result has nowhere to go.
func (d *doomed) kill() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.killed {
		return
	}
	d.killed = true
	d.c.Close()
	for _, wk := range d.workers {
		wk.Stop()
	}
}

func (d *doomed) ran() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runs
}

func dialWorker(t *testing.T, addr string) client.Client {
	t.Helper()
	c, err := client.Dial(client.Options{HostPort: addr, Logger: discardSDKLogger()})
	if err != nil {
		t.Fatalf("dialling Temporal at %s: %v", addr, err)
	}
	t.Cleanup(c.Close)
	return c
}

func mainWorker(c client.Client, queue string, w *world) worker.Worker {
	wk := worker.New(c, queue, worker.Options{})
	wk.RegisterWorkflow(FeatureWorkflow)
	wk.RegisterWorkflow(TaskWorkflow)
	w.registerOn(wk)
	return wk
}

func attemptWorker(c client.Client, queue string, runAttempt any, w *world) worker.Worker {
	wk := worker.New(c, AttemptQueue(queue), worker.Options{MaxConcurrentActivityExecutionSize: 1})
	wk.RegisterActivityWithOptions(runAttempt, sdkactivity.RegisterOptions{Name: "RunAttempt"})
	wk.RegisterActivityWithOptions(w.Finalize, sdkactivity.RegisterOptions{Name: "Finalize"})
	return wk
}

func startWorkers(t *testing.T, workers ...worker.Worker) {
	t.Helper()
	for _, wk := range workers {
		if err := wk.Start(); err != nil {
			t.Fatal(err)
		}
	}
}

func callCounts(w *world, names ...string) map[string]int {
	out := map[string]int{}
	for _, n := range names {
		out[n] = w.called(n)
	}
	return out
}

func discardSDKLogger() tlog.Logger {
	return tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}
