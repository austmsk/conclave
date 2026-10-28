package activity

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// TestParseIssue derives the single task from owner-written sections,
// and names the missing section when it cannot.
func TestParseIssue(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantDesc    string
		wantAC      []string
		wantProblem string
	}{
		{
			name:     "all three sections, any case, with a trailing colon",
			body:     "intro\n\n## Task\nExport tallies as CSV.\n\n### acceptance criteria:\n- one row per precinct\n- [ ] a header row\n  continued here\n1. totals match\n\n## Hints\nSee Tally.java\n",
			wantDesc: "Export tallies as CSV.\n\nHints:\nSee Tally.java",
			wantAC:   []string{"one row per precinct", "a header row continued here", "totals match"},
		},
		{
			name:     "a deeper heading stays in its section",
			body:     "# Task\nDo it.\n## Detail\nmore\n# Acceptance Criteria\n* works\n",
			wantDesc: "Do it.\n## Detail\nmore",
			wantAC:   []string{"works"},
		},
		{
			name:        "a heading inside a code fence is not a section",
			body:        "```\n## Task\nfake\n```\n## Acceptance criteria\n- x\n",
			wantProblem: "no Task section",
		},
		{
			name:        "no task",
			body:        "## Acceptance criteria\n- x\n",
			wantProblem: "no Task section",
		},
		{
			name:        "criteria without list items",
			body:        "## Task\nDo it.\n## Acceptance criteria\nit should work\n",
			wantProblem: "Acceptance criteria section is missing or has no list items",
		},
		{
			name:     "a repeated section keeps the first",
			body:     "## Task\nfirst\n## Task\nsecond\n## Acceptance criteria\n- x\n",
			wantDesc: "first",
			wantAC:   []string{"x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, problem := ParseIssue(tt.body)
			if tt.wantProblem != "" {
				if !strings.Contains(problem, tt.wantProblem) {
					t.Fatalf("problem = %q, want it to mention %q", problem, tt.wantProblem)
				}
				return
			}
			if problem != "" {
				t.Fatalf("problem = %q", problem)
			}
			if spec.Key != TaskKey || spec.Description != tt.wantDesc {
				t.Errorf("spec = %q %q, want %q %q", spec.Key, spec.Description, TaskKey, tt.wantDesc)
			}
			if len(spec.Criteria) != len(tt.wantAC) {
				t.Fatalf("criteria = %+v, want %q", spec.Criteria, tt.wantAC)
			}
			for i, c := range spec.Criteria {
				want := contracts.AcceptanceCriteria{ID: "AC" + string(rune('1'+i)), Text: tt.wantAC[i]}
				if c != want {
					t.Errorf("criterion %d = %+v, want %+v", i, c, want)
				}
			}
		})
	}
}

// TestPlanCommentCarriesMarker checks the plan comment ends with the
// marker the idempotent post searches for, as visible text.
func TestPlanCommentCarriesMarker(t *testing.T) {
	const id = "22222222-2222-4222-8222-222222222222"
	body := planComment(id, PlanBody{Task: contracts.TaskSpec{Key: "T1", Description: "d"}, Issue: PlanIssue{Number: 3}})
	if !strings.Contains(body, planMarker(id)) {
		t.Errorf("plan comment has no marker:\n%s", body)
	}
	if strings.Contains(body, "<!--") {
		t.Error("marker must be visible text: the client strips HTML comments")
	}
}

// TestHeartbeat beats on its interval and never after stop returns.
func TestHeartbeat(t *testing.T) {
	var beats atomic.Int32
	stop := startHeartbeat(context.Background(), 5*time.Millisecond, func(context.Context) { beats.Add(1) })
	time.Sleep(40 * time.Millisecond)
	stop()
	after := beats.Load()
	if after == 0 {
		t.Fatal("no heartbeat in 40ms at a 5ms interval")
	}
	time.Sleep(20 * time.Millisecond)
	if beats.Load() != after {
		t.Error("heartbeat after stop")
	}
	stop()
}
