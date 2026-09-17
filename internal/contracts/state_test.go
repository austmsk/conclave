package contracts

import (
	"slices"
	"testing"
)

// legal is the documented lifecycle, written out by hand rather than derived
// from the table under test. Every (kind, from, to) triple is checked against
// it, so a transition added to the table without being added here fails, and
// vice versa.
var legal = map[EntityKind]map[string][]string{
	EntityFeatureRequest: {
		"planning":          {"awaiting_approval", "needs_attention", "cancelled"},
		"awaiting_approval": {"executing", "planning", "cancelled"},
		"executing":         {"delivered", "needs_attention", "cancelled"},
		"needs_attention":   {"executing", "planning", "cancelled"},
		"delivered":         {},
		"cancelled":         {},
	},
	EntityPlan: {
		"proposed":   {"approved", "superseded", "rejected"},
		"approved":   {"superseded"},
		"superseded": {},
		"rejected":   {},
	},
	EntityTask: {
		"running":         {"done", "needs_attention", "cancelled"},
		"needs_attention": {"running", "cancelled"},
		"done":            {},
		"cancelled":       {},
	},
	EntityAttempt: {
		"running": {"passed", "failed", "errored"},
		"passed":  {},
		"failed":  {},
		"errored": {},
	},
}

func kinds() []EntityKind {
	return []EntityKind{EntityFeatureRequest, EntityPlan, EntityTask, EntityAttempt}
}

func TestStatesMatchDocumentedLifecycle(t *testing.T) {
	for _, kind := range kinds() {
		want := make([]string, 0, len(legal[kind]))
		for s := range legal[kind] {
			want = append(want, s)
		}
		slices.Sort(want)

		if got := States(kind); !slices.Equal(got, want) {
			t.Errorf("States(%s) = %v, want %v", kind, got, want)
		}
	}
	if got := States("no_such_kind"); got != nil {
		t.Errorf("States(unknown) = %v, want nil", got)
	}
}

// TestCanTransitionExhaustive checks every ordered pair of states for every
// kind, so both directions of error are caught: a legal move reported as
// illegal and an illegal move reported as legal.
func TestCanTransitionExhaustive(t *testing.T) {
	for _, kind := range kinds() {
		states := States(kind)
		for _, from := range states {
			for _, to := range states {
				want := slices.Contains(legal[kind][from], to)
				if got := CanTransition(kind, from, to); got != want {
					t.Errorf("CanTransition(%s, %s, %s) = %v, want %v", kind, from, to, got, want)
				}
			}
		}
	}
}

func TestCanTransitionRejectsUnknowns(t *testing.T) {
	cases := []struct {
		name string
		kind EntityKind
		from string
		to   string
	}{
		{"unknown kind", "no_such_kind", "running", "done"},
		{"unknown from", EntityTask, "pending", "running"},
		{"unknown to", EntityTask, "running", "integrated"},
		{"empty from", EntityFeatureRequest, "", "planning"},
		{"empty to", EntityFeatureRequest, "planning", ""},
		{"self transition", EntityAttempt, "running", "running"},
		{"terminal self transition", EntityAttempt, "passed", "passed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if CanTransition(tc.kind, tc.from, tc.to) {
				t.Errorf("CanTransition(%s, %q, %q) = true, want false", tc.kind, tc.from, tc.to)
			}
		})
	}
}

func TestTypedCanMoveToAgreesWithTable(t *testing.T) {
	if !FeaturePlanning.CanMoveTo(FeatureAwaitingApproval) {
		t.Error("planning -> awaiting_approval should be legal")
	}
	if FeatureDelivered.CanMoveTo(FeatureExecuting) {
		t.Error("delivered is terminal")
	}
	if !PlanProposed.CanMoveTo(PlanApproved) {
		t.Error("proposed -> approved should be legal")
	}
	if !TaskNeedsAttention.CanMoveTo(TaskRunning) {
		t.Error("needs_attention -> running (retry) should be legal")
	}
	if AttemptErrored.CanMoveTo(AttemptRunning) {
		t.Error("errored attempts are never resumed; a retry is a new attempt")
	}
}
