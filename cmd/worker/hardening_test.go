package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
	"github.com/austmsk/conclave/internal/workflow"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestWorkerRefusesStartWhileHalted: startup stops while a halt stands,
// and the error says how to clear it.
func TestWorkerRefusesStartWhileHalted(t *testing.T) {
	halted := func(context.Context) (store.HaltState, error) {
		return store.HaltState{Halted: true, ID: 4, Since: time.Now(), Reason: "drill"}, nil
	}
	if err := refuseIfHalted(t.Context(), halted); !errors.Is(err, errHalted) {
		t.Fatalf("got %v, want errHalted", err)
	}
	running := func(context.Context) (store.HaltState, error) { return store.HaltState{ID: 5}, nil }
	if err := refuseIfHalted(t.Context(), running); err != nil {
		t.Fatalf("not halted: %v", err)
	}
	broken := func(context.Context) (store.HaltState, error) { return store.HaltState{}, errors.New("db down") }
	if err := refuseIfHalted(t.Context(), broken); err == nil {
		t.Fatal("an unreadable kill switch let the worker start")
	}
}

// TestWatcherStopsAfterGrace: the watcher sees a halt on a later poll,
// keeps the worker running for the grace period, then stops it once.
func TestWatcherStopsAfterGrace(t *testing.T) {
	var polls atomic.Int32
	var haltedAt atomic.Int64
	state := func(context.Context) (store.HaltState, error) {
		if polls.Add(1) < 3 {
			return store.HaltState{}, nil
		}
		haltedAt.CompareAndSwap(0, time.Now().UnixNano())
		return store.HaltState{Halted: true, ID: 1}, nil
	}
	var stops atomic.Int32
	var stoppedAt time.Time
	done := make(chan struct{})
	go func() {
		watchHalt(t.Context(), state, time.Millisecond, 50*time.Millisecond, func() { stops.Add(1); stoppedAt = time.Now() }, quietLog())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never stopped the worker")
	}
	if stops.Load() != 1 {
		t.Fatalf("stop called %d times", stops.Load())
	}
	if waited := stoppedAt.Sub(time.Unix(0, haltedAt.Load())); waited < 50*time.Millisecond {
		t.Fatalf("stopped %s after the halt, before the grace period", waited)
	}
}

// TestWatcherKeepsPollingThroughReadErrors: a database blip neither stops
// the worker nor ends the watch.
func TestWatcherKeepsPollingThroughReadErrors(t *testing.T) {
	var polls atomic.Int32
	state := func(context.Context) (store.HaltState, error) {
		if polls.Add(1) < 4 {
			return store.HaltState{}, errors.New("db down")
		}
		return store.HaltState{Halted: true}, nil
	}
	stopped := make(chan struct{})
	go watchHalt(t.Context(), state, time.Millisecond, 0, func() { close(stopped) }, quietLog())
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher gave up after read errors")
	}
}

// TestCheckSweepAfterCoversFinalize: with an attempt shorter than
// Finalize, the cutoff must still exceed Finalize's 45 minutes (ticket 9
// review item).
func TestCheckSweepAfterCoversFinalize(t *testing.T) {
	quick := agent.Implementer
	quick.Limits.MaxWallClock = time.Minute
	d := activity.Deps{
		Roles: map[contracts.Role]contracts.RoleSpec{contracts.RoleImplementer: quick},
		Repos: map[string]activity.Repo{"octo/tally": {}},
	}
	if d.LongestAttempt() >= activity.FinalizeTimeout {
		t.Fatalf("fixture attempt %s is not shorter than Finalize", d.LongestAttempt())
	}
	if err := checkSweepAfter(40*time.Minute, d.LongestSandboxHold()); err == nil {
		t.Fatal("a cutoff below Finalize's bound was accepted")
	}
	if err := checkSweepAfter(sweepAfter, d.LongestSandboxHold()); err != nil {
		t.Fatalf("default cutoff refused: %v", err)
	}
}

// TestCheckAlerts: the worker needs an alerts repository, and refuses one
// that agents also run on.
func TestCheckAlerts(t *testing.T) {
	alerts := github.AlertsConfig{Repository: "octo/ops", RepositoryID: 9, Assignee: "octo"}
	tests := []struct {
		name    string
		alerts  github.AlertsConfig
		runs    string
		wantErr bool
	}{
		{"own repository", alerts, "octo/tally", false},
		{"missing", github.AlertsConfig{}, "octo/tally", true},
		{"also a run repository", alerts, "octo/ops", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := deploy{GitHub: github.Config{Alerts: tt.alerts}, Access: map[string]llm.RepoAccess{tt.runs: {}}}
			if err := checkAlerts(d); (err != nil) != tt.wantErr {
				t.Fatalf("checkAlerts = %v, want error %t", err, tt.wantErr)
			}
		})
	}
}

// fakeSchedules is the Temporal schedule client's create and update, in
// memory. Its other methods are never called.
type fakeSchedules struct {
	client.ScheduleClient
	mu      sync.Mutex
	created map[string]client.ScheduleOptions
	current map[string]*client.Schedule
	updates int
}

func newFakeSchedules() *fakeSchedules {
	return &fakeSchedules{created: map[string]client.ScheduleOptions{}, current: map[string]*client.Schedule{}}
}

func (f *fakeSchedules) Create(_ context.Context, o client.ScheduleOptions) (client.ScheduleHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.created[o.ID]; ok {
		return nil, temporal.ErrScheduleAlreadyRunning
	}
	f.created[o.ID] = o
	spec := o.Spec
	f.current[o.ID] = &client.Schedule{
		Spec: &spec, Action: o.Action, Policy: &client.SchedulePolicies{Overlap: o.Overlap}, State: &client.ScheduleState{},
	}
	return nil, nil
}

func (f *fakeSchedules) GetHandle(_ context.Context, id string) client.ScheduleHandle {
	return &fakeScheduleHandle{f: f, id: id}
}

type fakeScheduleHandle struct {
	client.ScheduleHandle
	f  *fakeSchedules
	id string
}

func (h *fakeScheduleHandle) Update(_ context.Context, o client.ScheduleUpdateOptions) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	u, err := o.DoUpdate(client.ScheduleUpdateInput{Description: client.ScheduleDescription{Schedule: *h.f.current[h.id]}})
	if err != nil {
		return err
	}
	h.f.current[h.id] = u.Schedule
	h.f.updates++
	return nil
}

// TestEnsureReaperScheduleIdempotent starts the worker twice, the second
// time with a new interval while the schedule is paused by a halt: one
// schedule, updated in place, still paused, overlap skipped.
func TestEnsureReaperScheduleIdempotent(t *testing.T) {
	sc := newFakeSchedules()
	if err := ensureReaperSchedule(t.Context(), sc, 15*time.Minute, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	sc.current[workflow.ReaperScheduleID].State.Paused = true
	if err := ensureReaperSchedule(t.Context(), sc, 20*time.Minute, 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(sc.created) != 1 || sc.updates != 1 {
		t.Fatalf("%d created, %d updates; want one schedule updated once", len(sc.created), sc.updates)
	}
	s := sc.current[workflow.ReaperScheduleID]
	if got := s.Spec.Intervals[0].Every; got != 20*time.Minute {
		t.Errorf("interval %s, want the new one", got)
	}
	action := s.Action.(*client.ScheduleWorkflowAction)
	if in := action.Args[0].(workflow.ReaperInput); in.OlderThan != 3*time.Hour || action.TaskQueue != workflow.TaskQueue {
		t.Errorf("action %+v, want the new cutoff on the workflow queue", action)
	}
	if !s.State.Paused || s.Policy.Overlap != enumspb.SCHEDULE_OVERLAP_POLICY_SKIP {
		t.Errorf("state %+v, policy %+v; want still paused, overlap skipped", s.State, s.Policy)
	}
}

func TestEnsureReaperScheduleRefusesTightInterval(t *testing.T) {
	if err := ensureReaperSchedule(t.Context(), newFakeSchedules(), time.Second, 2*time.Hour); err == nil {
		t.Fatal("a one-second reaper was accepted")
	}
}
