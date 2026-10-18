package agent

import (
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

// invalidFinish is a finish whose report lacks its summary.
func invalidFinish(t *testing.T, id string) contracts.ToolCall {
	t.Helper()
	return callOf(id, contracts.ToolFinish, reportWith(t, "summary", nil))
}

func TestFinishRepairThenValid(t *testing.T) {
	router, provider := agenttest.Router(t, turnCalls(invalidFinish(t, "c1")), turnCalls(finishCall("c2")))
	g := newRig(t, router)
	out, err := g.run(t, testSpec(nil))
	if err != nil || out.Reason != "" {
		t.Fatalf("Run: %q, %v; want completion", out.Reason, err)
	}
	if out.Report.Summary != "Count ballots per precinct" {
		t.Errorf("report %+v", out.Report)
	}
	repair := lastResults(t, provider.Requests()[1])
	if len(repair) != 1 || !strings.Contains(repair[0].Error, "report.summary is required") {
		t.Errorf("repair result %+v does not carry the validation message", repair)
	}
	checkSteps(t, g.rec.all(), codeInvalidReport, "")
}

func TestFinishInvalidTwice(t *testing.T) {
	router, provider := agenttest.Router(t,
		turnCalls(invalidFinish(t, "c1")), turnCalls(invalidFinish(t, "c2")), turnCalls(finishCall("c3")))
	g := newRig(t, router)
	out, err := g.run(t, testSpec(nil))
	if err != nil || out.Reason != contracts.ReasonInvalidOutput {
		t.Fatalf("Run: %q, %v; want invalid_output", out.Reason, err)
	}
	if provider.Remaining() != 1 {
		t.Errorf("%d turns left, want 1: there is one repair, not two", provider.Remaining())
	}
	checkSteps(t, g.rec.all(), codeInvalidReport, codeInvalidReport)
}

func TestRepairIsOnlyOnceAcrossTurns(t *testing.T) {
	cases := []struct {
		name  string
		turns func(t *testing.T) []faketest.Turn
	}{
		{"invalid report, work, invalid report", func(t *testing.T) []faketest.Turn {
			return []faketest.Turn{turnCalls(invalidFinish(t, "c1")), turnCalls(readCall("c2", "a.txt")),
				turnCalls(invalidFinish(t, "c3")), turnCalls(finishCall("c4"))}
		}},
		{"no tool call, work, invalid report", func(t *testing.T) []faketest.Turn {
			return []faketest.Turn{turnText(contracts.StopEndTurn, "Done."), turnCalls(readCall("c2", "a.txt")),
				turnCalls(invalidFinish(t, "c3")), turnCalls(finishCall("c4"))}
		}},
		{"invalid report, work, no tool call", func(t *testing.T) []faketest.Turn {
			return []faketest.Turn{turnCalls(invalidFinish(t, "c1")), turnCalls(readCall("c2", "a.txt")),
				turnText(contracts.StopEndTurn, "Done."), turnCalls(finishCall("c4"))}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, provider := agenttest.Router(t, tc.turns(t)...)
			g := newRig(t, router)
			seedFiles(t, g.sb, map[string]string{"a.txt": "a"})
			out, err := g.run(t, testSpec(nil))
			if err != nil || out.Reason != contracts.ReasonInvalidOutput {
				t.Fatalf("Run: %q, %v; want invalid_output", out.Reason, err)
			}
			if provider.Remaining() != 1 {
				t.Errorf("%d turns left, want 1", provider.Remaining())
			}
		})
	}
}

func TestEndTurnWithoutFinish(t *testing.T) {
	cases := []struct {
		name   string
		turns  []faketest.Turn
		reason contracts.FailureReason
		left   int
	}{
		{"nudged, then finishes", []faketest.Turn{turnText(contracts.StopEndTurn, "All done."), turnCalls(finishCall("c1"))}, "", 0},
		{"end_turn twice", []faketest.Turn{turnText(contracts.StopEndTurn, "All done."), turnText(contracts.StopEndTurn, "Really."),
			turnCalls(finishCall("c1"))}, contracts.ReasonInvalidOutput, 1},
		{"max_tokens, then end_turn", []faketest.Turn{turnText(contracts.StopMaxTokens, "I was about to"),
			turnText(contracts.StopEndTurn, "Done."), turnCalls(finishCall("c1"))}, contracts.ReasonInvalidOutput, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, provider := agenttest.Router(t, tc.turns...)
			g := newRig(t, router)
			out, err := g.run(t, testSpec(nil))
			if err != nil || out.Reason != tc.reason {
				t.Fatalf("Run: %q, %v; want %q", out.Reason, err, tc.reason)
			}
			if provider.Remaining() != tc.left {
				t.Errorf("%d turns left, want %d", provider.Remaining(), tc.left)
			}
			nudged := provider.Requests()[1].Messages
			last := nudged[len(nudged)-1]
			if last.Role != contracts.MessageUser || last.Text != finishNudge {
				t.Errorf("second request ends with %+v, want the finish nudge", last)
			}
		})
	}
}

// TestFinishAlongsideOtherCalls: a finish sent with other calls is
// refused, the others run, and the repair is still unused.
func TestFinishAlongsideOtherCalls(t *testing.T) {
	router, provider := agenttest.Router(t,
		turnCalls(readCall("c1", "a.txt"), finishCall("c2")),
		turnCalls(invalidFinish(t, "c3")),
		turnCalls(finishCall("c4")),
	)
	g := newRig(t, router)
	seedFiles(t, g.sb, map[string]string{"a.txt": "alpha"})
	out, err := g.run(t, testSpec(nil))
	if err != nil || out.Reason != "" {
		t.Fatalf("Run: %q, %v; want completion after the repair", out.Reason, err)
	}
	results := lastResults(t, provider.Requests()[1])
	if len(results) != 2 || results[0].Output != "alpha" || !strings.Contains(results[1].Error, "only tool call") {
		t.Errorf("results %+v: want the read answered and the finish refused", results)
	}
	checkSteps(t, g.rec.all(), "", codeFinishNotAlone, codeInvalidReport, "")
}
