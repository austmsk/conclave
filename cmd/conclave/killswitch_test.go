package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/alert"
	"github.com/austmsk/conclave/internal/store"
)

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// fakeRuns is Temporal's open workflows, in memory. listings[i] is what
// the ith listing returns, the last repeating; cancelling a run removes it
// from every later listing unless sticky holds its ID.
type fakeRuns struct {
	mu        sync.Mutex
	calls     *[]string
	listings  [][]run
	listed    int
	cancelled []string
	closed    map[string]bool
	sticky    map[string]bool
	failPause error
}

func (f *fakeRuns) open(context.Context) ([]run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.calls = append(*f.calls, "list")
	i := min(f.listed, len(f.listings)-1)
	f.listed++
	var out []run
	for _, r := range f.listings[i] {
		if !f.closed[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeRuns) cancel(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.calls = append(*f.calls, "cancel "+id)
	f.cancelled = append(f.cancelled, id)
	if !f.sticky[id] {
		f.closed[id] = true
	}
	return nil
}

func (f *fakeRuns) pauseReaper(context.Context, string) error {
	*f.calls = append(*f.calls, "pause")
	return f.failPause
}

func (f *fakeRuns) unpauseReaper(context.Context, string) error {
	*f.calls = append(*f.calls, "unpause")
	return nil
}

// harness is a kill switch over fakes that records every step in calls.
type harness struct {
	k        *killSwitch
	runs     *fakeRuns
	calls    []string
	halt     store.HaltState
	alerts   []alert.Alert
	out      bytes.Buffer
	left     [2]int
	raiseErr error
}

func newHarness(listings ...[]run) *harness {
	h := &harness{}
	clock := t0
	h.runs = &fakeRuns{calls: &h.calls, listings: listings, closed: map[string]bool{}, sticky: map[string]bool{}}
	if len(listings) == 0 {
		h.runs.listings = [][]run{nil}
	}
	h.k = &killSwitch{
		setHalt: func(_ context.Context, r string) (store.HaltState, error) {
			h.calls = append(h.calls, "set")
			if !h.halt.Halted {
				h.halt = store.HaltState{Halted: true, ID: h.halt.ID + 1, Since: clock, Reason: r}
			}
			return h.halt, nil
		},
		clearHalt: func(_ context.Context, r string) (store.HaltState, error) {
			h.calls = append(h.calls, "clear")
			h.halt.Halted, h.halt.Reason = false, r
			return h.halt, nil
		},
		currentHalt: func(context.Context) (store.HaltState, error) { return h.halt, nil },
		runs:        h.runs,
		sweep: func(context.Context) (activity.SweepResult, error) {
			h.calls = append(h.calls, "sweep")
			return activity.SweepResult{Released: 1, Reaped: 2}, nil
		},
		remaining: func(context.Context) (int, int, error) {
			h.calls = append(h.calls, "remaining")
			return h.left[0], h.left[1], nil
		},
		raise: func(_ context.Context, a alert.Alert) error {
			h.calls = append(h.calls, "alert "+a.OccurrenceID)
			h.alerts = append(h.alerts, a)
			return h.raiseErr
		},
		drain: time.Minute, poll: time.Second,
		now:   func() time.Time { return clock },
		sleep: func(_ context.Context, d time.Duration) error { clock = clock.Add(d); return nil },
		out:   &h.out,
	}
	return h
}

func TestHaltOrder(t *testing.T) {
	h := newHarness([]run{{ID: "feature/1/7", RunID: "r"}})
	if err := h.k.halt(t.Context(), "drill"); err != nil {
		t.Fatal(err)
	}
	// Two empty listings in a row end the drain. The result alert is its
	// own occurrence, named by the time of this halt run.
	want := []string{"set", "alert halt:1", "pause", "list", "cancel feature/1/7", "list", "list", "sweep", "remaining"}
	n := len(h.calls)
	if n != len(want)+1 || !slices.Equal(h.calls[:n-1], want) || !strings.HasPrefix(h.calls[n-1], "alert halt:1:done:") {
		t.Fatalf("steps\n %v\nwant\n %v then alert halt:1:done:<time>", h.calls, want)
	}
	done := h.alerts[1]
	if !strings.Contains(done.Action, "1 workflows cancelled, 0 still open; 1 recorded sandboxes released, 2 unrecorded containers reaped") {
		t.Fatalf("halt result %q", done.Action)
	}
}

// TestHaltTwice: a second halt keeps the first's ID, so its alerts are the
// same occurrences, and finds nothing left to cancel.
func TestHaltTwice(t *testing.T) {
	h := newHarness([]run{{ID: "feature/1/7"}})
	for range 2 {
		if err := h.k.halt(t.Context(), "drill"); err != nil {
			t.Fatal(err)
		}
	}
	if h.halt.ID != 1 || len(h.runs.cancelled) != 1 {
		t.Fatalf("halt %d, cancelled %v; want one halt and one cancel", h.halt.ID, h.runs.cancelled)
	}
	if h.alerts[0].OccurrenceID != h.alerts[2].OccurrenceID {
		t.Fatalf("second halt raised %s, want the first's %s", h.alerts[2].OccurrenceID, h.alerts[0].OccurrenceID)
	}
}

// TestHaltCancelsLateStarts: a workflow a label starts mid-halt shows up
// on a later listing and is cancelled too.
func TestHaltCancelsLateStarts(t *testing.T) {
	first := []run{{ID: "feature/1/7"}}
	second := []run{{ID: "feature/1/7"}, {ID: "feature/1/8"}}
	h := newHarness(first, second)
	h.runs.sticky["feature/1/7"] = true // still cleaning up
	h.runs.listings = append(h.runs.listings, nil)
	if err := h.k.halt(t.Context(), "drill"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.runs.cancelled, []string{"feature/1/7", "feature/1/8"}) {
		t.Fatalf("cancelled %v, want each once, the late one included", h.runs.cancelled)
	}
}

// TestHaltCancelsRootsOnly: a task child is left to its parent while the
// parent is open, and cancelled directly once the parent is gone.
func TestHaltCancelsRootsOnly(t *testing.T) {
	parent := run{ID: "feature/1/7"}
	child := run{ID: "feature/1/7/task/x", ParentID: "feature/1/7"}
	orphan := run{ID: "feature/1/9/task/y", ParentID: "feature/1/9"}
	h := newHarness([]run{parent, child, orphan}, nil)
	if err := h.k.halt(t.Context(), "drill"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.runs.cancelled, []string{"feature/1/7", "feature/1/9/task/y"}) {
		t.Fatalf("cancelled %v, want the parent and the parentless child", h.runs.cancelled)
	}
}

// TestHaltDeliveryFailureStillHalts: GitHub down stops neither the
// cancellations nor the sweep.
func TestHaltDeliveryFailureStillHalts(t *testing.T) {
	h := newHarness([]run{{ID: "feature/1/7"}})
	h.raiseErr = errors.New("github: unavailable")
	h.runs.failPause = errors.New("temporal: schedule unavailable")
	if err := h.k.halt(t.Context(), "drill"); err != nil {
		t.Fatalf("halt failed on delivery: %v", err)
	}
	if !slices.Contains(h.calls, "cancel feature/1/7") || !slices.Contains(h.calls, "sweep") {
		t.Fatalf("steps %v skipped work after a delivery failure", h.calls)
	}
	if !strings.Contains(h.out.String(), "warning: halt alert") || !strings.Contains(h.out.String(), "warning: pausing the reaper") {
		t.Fatalf("output does not warn:\n%s", h.out.String())
	}
}

// TestHaltReportsRemaining: anything still open or still there after the
// sweep makes the halt fail, so its exit status says so.
func TestHaltReportsRemaining(t *testing.T) {
	tests := map[string]func(*harness){
		"labelled container": func(h *harness) { h.left = [2]int{0, 1} },
		"unreleased attempt": func(h *harness) { h.left = [2]int{1, 0} },
		"workflow never closes": func(h *harness) {
			h.runs.sticky["feature/1/7"] = true
		},
	}
	for name, set := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness([]run{{ID: "feature/1/7"}})
			set(h)
			if err := h.k.halt(t.Context(), "drill"); !errors.Is(err, errIncomplete) {
				t.Fatalf("got %v, want errIncomplete", err)
			}
		})
	}
}

func TestResumeCancelsStartedDuringHalt(t *testing.T) {
	h := newHarness([]run{{ID: "before", Start: t0.Add(-time.Hour)}, {ID: "during", Start: t0.Add(time.Minute)}})
	h.halt = store.HaltState{Halted: true, ID: 3, Since: t0}
	if err := h.k.resume(t.Context(), "drill over", false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.runs.cancelled, []string{"during"}) {
		t.Fatalf("cancelled %v, want only the run started during the halt", h.runs.cancelled)
	}
	if h.halt.Halted || !slices.Contains(h.calls, "unpause") {
		t.Fatalf("halt %+v, steps %v; want cleared and the reaper unpaused", h.halt, h.calls)
	}
	if i, j := slices.Index(h.calls, "cancel during"), slices.Index(h.calls, "clear"); i > j {
		t.Fatalf("steps %v: cleared before cancelling", h.calls)
	}
}

func TestResumeKeep(t *testing.T) {
	h := newHarness([]run{{ID: "during", Start: t0.Add(time.Minute)}})
	h.halt = store.HaltState{Halted: true, ID: 3, Since: t0}
	if err := h.k.resume(t.Context(), "keep it", true); err != nil {
		t.Fatal(err)
	}
	if len(h.runs.cancelled) != 0 || h.halt.Halted {
		t.Fatalf("cancelled %v, halted %t; want nothing cancelled and the halt cleared", h.runs.cancelled, h.halt.Halted)
	}
	if !strings.Contains(h.out.String(), "started during the halt: during") {
		t.Fatalf("the kept run was not listed:\n%s", h.out.String())
	}
}

func TestResumeWhenNotHalted(t *testing.T) {
	h := newHarness([]run{{ID: "x", Start: t0}})
	if err := h.k.resume(t.Context(), "nothing", false); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("steps %v, want none", h.calls)
	}
}

// TestHaltRecordedWhenEverythingElseIsDown: with Temporal, Docker and
// GitHub all unreachable, the halt is still recorded first, every later
// step is attempted and reported, and the halt says it is incomplete.
func TestHaltRecordedWhenEverythingElseIsDown(t *testing.T) {
	h := newHarness()
	h.k.unavailable(errors.New("down"))
	h.k.connect = func(context.Context) { h.calls = append(h.calls, "connect") }
	err := h.k.halt(t.Context(), "drill")
	if !errors.Is(err, errIncomplete) {
		t.Fatalf("got %v, want errIncomplete", err)
	}
	if !h.halt.Halted || len(h.calls) < 2 || h.calls[0] != "set" || h.calls[1] != "connect" {
		t.Fatalf("halt %+v, steps %v; want the halt recorded, then connect", h.halt, h.calls)
	}
	for _, want := range []string{"warning: halt alert", "warning: pausing the reaper", "temporal unavailable: down", "docker unavailable: down"} {
		if !strings.Contains(h.out.String()+err.Error(), want) {
			t.Errorf("output lacks %q:\n%s\n%v", want, h.out.String(), err)
		}
	}
}

// TestHaltReasonRedacted: a token typed into -reason never reaches the
// append-only halt_events table, the alert, or the clear.
func TestHaltReasonRedacted(t *testing.T) {
	token := "gh" + "s_" + strings.Repeat("Z9y8", 9)
	h := newHarness()
	if err := h.k.halt(t.Context(), "rotating "+token); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.halt.Reason, token) || strings.Contains(h.alerts[0].What, token) {
		t.Fatalf("token kept: reason %q, alert %q", h.halt.Reason, h.alerts[0].What)
	}
	if err := h.k.resume(t.Context(), "done with "+token, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.halt.Reason, token) {
		t.Fatalf("clear kept the token: %q", h.halt.Reason)
	}
}
