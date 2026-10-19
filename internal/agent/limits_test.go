package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

// withSteps sets MaxSteps.
func withSteps(n int) func(*contracts.RoleSpec) {
	return func(s *contracts.RoleSpec) { s.Limits.MaxSteps = n }
}

func TestLimitMaxStepsBoundary(t *testing.T) {
	cases := []struct {
		name   string
		turns  []faketest.Turn
		reason contracts.FailureReason
		codes  []string
	}{
		{
			name:  "N calls pass",
			turns: []faketest.Turn{turnCalls(readCall("c1", "a.txt")), turnCalls(readCall("c2", "a.txt")), turnCalls(finishCall("c3"))},
			codes: []string{"", "", ""},
		},
		{
			name: "call N+1 fails",
			turns: []faketest.Turn{turnCalls(readCall("c1", "a.txt")), turnCalls(readCall("c2", "a.txt")),
				turnCalls(readCall("c3", "a.txt")), turnCalls(finishCall("c4"))},
			reason: contracts.ReasonStepLimit,
			codes:  []string{"", "", "", "not executed: step_limit"},
		},
		{
			name: "call N+1 in the same reply",
			turns: []faketest.Turn{turnCalls(readCall("c1", "a.txt"), readCall("c2", "a.txt")),
				turnCalls(readCall("c3", "a.txt"), readCall("c4", "a.txt"), readCall("c5", "a.txt"))},
			reason: contracts.ReasonStepLimit,
			codes:  []string{"", "", "", "not executed: step_limit", "not executed: step_limit"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, provider := agenttest.Router(t, tc.turns...)
			g := newRig(t, router)
			seedFiles(t, g.sb, map[string]string{"a.txt": "a"})
			out, err := g.run(t, testSpec(withSteps(3)))
			if err != nil || out.Reason != tc.reason {
				t.Fatalf("Run: %q, %v; want %q", out.Reason, err, tc.reason)
			}
			if provider.Remaining() != 0 {
				t.Errorf("%d turns unused", provider.Remaining())
			}
			if out.Usage.Steps != 3 || len(out.Steps) != len(tc.codes) {
				t.Errorf("usage steps %d, steps %d; want 3 and %d", out.Usage.Steps, len(out.Steps), len(tc.codes))
			}
			if reads := len(g.sb.Calls()); reads > 3 {
				t.Errorf("%d sandbox calls with a limit of 3 steps", reads)
			}
			checkSteps(t, g.rec.all(), tc.codes...)
		})
	}
}

func usageTurn(input, output int, calls ...contracts.ToolCall) faketest.Turn {
	tr := turnCalls(calls...)
	tr.Response.InputTokens, tr.Response.OutputTokens = input, output
	return tr
}

// TestLimitMaxTokensRunner: the router's budget is large, so only the
// runner's own MaxTokens can stop these sessions.
func TestLimitMaxTokensRunner(t *testing.T) {
	cases := []struct {
		name string
		// second is the second reply's input and output tokens.
		second [2]int
		// reads is how many of the two reads run.
		reads int
		codes []string
	}{
		{"reply passes the limit", [2]int{500, 200}, 1, []string{"", "not executed: token_limit"}},
		{"reply reaches the limit exactly", [2]int{400, 200}, 2, []string{"", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, provider := agenttest.Router(t,
				usageTurn(300, 100, readCall("c1", "a.txt")),
				usageTurn(tc.second[0], tc.second[1], readCall("c2", "a.txt")),
				turnCalls(finishCall("c3")),
			)
			g := newRig(t, router)
			seedFiles(t, g.sb, map[string]string{"a.txt": "a"})
			out, err := g.run(t, testSpec(func(s *contracts.RoleSpec) { s.Limits.MaxTokens = 1000 }))
			if err != nil || out.Reason != contracts.ReasonTokenLimit {
				t.Fatalf("Run: %q, %v; want token_limit", out.Reason, err)
			}
			if provider.Remaining() != 1 {
				t.Errorf("%d turns left, want 1: the session must stop before its third request", provider.Remaining())
			}
			reqs := provider.Requests()
			if len(reqs) != 2 || reqs[0].MaxTokens != 1000 || reqs[1].MaxTokens != 600 {
				t.Errorf("%d requests; want two, asking for 1000 then 600 tokens", len(reqs))
			}
			if n := len(g.sb.Calls()); n != tc.reads {
				t.Errorf("%d sandbox calls, want %d", n, tc.reads)
			}
			checkSteps(t, g.rec.all(), tc.codes...)
		})
	}
}

func TestLimitAttemptBudgetIsTokenLimit(t *testing.T) {
	router, provider, _ := agenttest.RouterWith(t, agenttest.Options{TokenBudget: 20_000},
		usageTurn(15_000, 5_000, readCall("c1", "a.txt")),
		turnCalls(finishCall("c2")),
	)
	g := newRig(t, router)
	seedFiles(t, g.sb, map[string]string{"a.txt": "a"})
	out, err := g.run(t, testSpec(nil))
	if err != nil || out.Reason != contracts.ReasonTokenLimit {
		t.Fatalf("Run: %q, %v; want token_limit and no error", out.Reason, err)
	}
	if provider.Remaining() != 1 {
		t.Errorf("%d turns left, want 1", provider.Remaining())
	}
}

func TestBudgetCapIsNotTokenLimit(t *testing.T) {
	cases := []struct {
		name    string
		budgets llm.Budgets
		kind    llm.BudgetKind
	}{
		{"feature request", llm.Budgets{FeatureRequestTokens: 10}, llm.BudgetFeatureRequest},
		{"month", llm.Budgets{MonthlyUSD: 1e-9}, llm.BudgetMonth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, provider, _ := agenttest.RouterWith(t, agenttest.Options{Budgets: tc.budgets}, turnCalls(finishCall("c1")))
			g := newRig(t, router)
			out, err := g.run(t, testSpec(nil))
			if !errors.Is(err, llm.ErrBudgetExceeded) {
				t.Fatalf("got %v, want a wrapped ErrBudgetExceeded", err)
			}
			var be llm.BudgetError
			if !errors.As(err, &be) || be.Kind != tc.kind {
				t.Errorf("budget kind %q, want %q", be.Kind, tc.kind)
			}
			if out.Reason != "" {
				t.Errorf("reason %q, want none: a platform cap is not the agent's failure", out.Reason)
			}
			if provider.Remaining() != 1 {
				t.Errorf("the provider was reached")
			}
		})
	}
}

// blockUntilDone is an Exec that runs until its context ends, as a
// command killed by the session's end would.
func blockUntilDone(ctx context.Context, _ contracts.ExecRequest) (contracts.ExecResult, error) {
	<-ctx.Done()
	return contracts.ExecResult{}, fmt.Errorf("exec interrupted: %w", context.Cause(ctx))
}

func TestLimitMaxWallClock(t *testing.T) {
	router, provider := agenttest.Router(t,
		turnCalls(callOf("c1", contracts.ToolRunTests, `{}`), readCall("c2", "a.txt")),
		turnCalls(finishCall("c3")),
	)
	g := newRig(t, router)
	g.sb.ExecFunc = blockUntilDone
	out, err := g.run(t, testSpec(func(s *contracts.RoleSpec) { s.Limits.MaxWallClock = 200 * time.Millisecond }))
	if err != nil || out.Reason != contracts.ReasonWallClock {
		t.Fatalf("Run: %q, %v; want wall_clock and no error", out.Reason, err)
	}
	if provider.Remaining() != 1 {
		t.Errorf("%d turns left, want 1", provider.Remaining())
	}
	// The interrupted call is recorded although the session's context had
	// ended, and so is the one that never ran.
	checkSteps(t, g.rec.all(), "wall_clock", "not executed: wall_clock")
}

func TestParentCancelIsNotWallClock(t *testing.T) {
	router, provider := agenttest.Router(t, turnCalls(callOf("c1", contracts.ToolRunTests, `{}`)), turnCalls(finishCall("c2")))
	g := newRig(t, router)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	g.sb.ExecFunc = func(ctx context.Context, req contracts.ExecRequest) (contracts.ExecResult, error) {
		cancel()
		return blockUntilDone(ctx, req)
	}
	out, err := g.runInput(t, ctx, testInput(testSpec(nil)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want a cancellation error", err)
	}
	if out.Reason != "" {
		t.Errorf("reason %q, want none: the caller cancelled", out.Reason)
	}
	if provider.Remaining() != 1 {
		t.Errorf("%d turns left, want 1", provider.Remaining())
	}
	checkSteps(t, g.rec.all(), codeCancelled)
}

// failingExec answers every command with the result next returns.
func failingExec(next func() contracts.ExecResult) func(context.Context, contracts.ExecRequest) (contracts.ExecResult, error) {
	return func(context.Context, contracts.ExecRequest) (contracts.ExecResult, error) { return next(), nil }
}

func TestLimitMaxRepeatErrors(t *testing.T) {
	// The same command with its arguments in a different order and layout
	// is the same call.
	router, provider := agenttest.Router(t,
		turnCalls(callOf("c1", contracts.ToolRunCommand, `{"command":["go","test"],"timeout_seconds":5}`)),
		turnCalls(callOf("c2", contracts.ToolRunCommand, `{"timeout_seconds":5, "command":["go","test"]}`)),
		turnCalls(callOf("c3", contracts.ToolRunCommand, `{ "command": ["go", "test"], "timeout_seconds": 5 }`), readCall("c4", "a.txt")),
		turnCalls(finishCall("c5")),
	)
	g := newRig(t, router)
	g.sb.ExecFunc = failingExec(func() contracts.ExecResult {
		return contracts.ExecResult{ExitCode: 1, Stdout: []byte("--- FAIL: TestCount")}
	})
	out, err := g.run(t, testSpec(nil))
	if err != nil || out.Reason != contracts.ReasonRepeatErrors {
		t.Fatalf("Run: %q, %v; want repeat_errors", out.Reason, err)
	}
	if provider.Remaining() != 1 {
		t.Errorf("%d turns left, want 1", provider.Remaining())
	}
	checkSteps(t, g.rec.all(), "", "", "", "not executed: repeat_errors")
}

func TestDistinctFailuresDoNotTrip(t *testing.T) {
	cases := []struct {
		name string
		next func(n int) contracts.ExecResult
	}{
		{"different output", func(n int) contracts.ExecResult {
			return contracts.ExecResult{ExitCode: 1, Stdout: fmt.Appendf(nil, "FAIL %d", n)}
		}},
		{"different exit code", func(n int) contracts.ExecResult {
			return contracts.ExecResult{ExitCode: n, Stdout: []byte("FAIL")}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tests := callOf("", contracts.ToolRunTests, `{}`)
			router, provider := agenttest.Router(t,
				turnCalls(withID(tests, "c1")), turnCalls(withID(tests, "c2")), turnCalls(withID(tests, "c3")),
				turnCalls(finishCall("c4")),
			)
			g := newRig(t, router)
			n := 0
			g.sb.ExecFunc = failingExec(func() contracts.ExecResult { n++; return tc.next(n) })
			out, err := g.run(t, testSpec(func(s *contracts.RoleSpec) { s.Limits.MaxRepeatErrors = 2 }))
			if err != nil || out.Reason != "" {
				t.Fatalf("Run: %q, %v; want completion", out.Reason, err)
			}
			if provider.Remaining() != 0 {
				t.Errorf("%d turns unused", provider.Remaining())
			}
		})
	}
}

func withID(c contracts.ToolCall, id string) contracts.ToolCall {
	c.ID = id
	return c
}

func TestLimitsMustBePositive(t *testing.T) {
	cases := []struct {
		name string
		edit func(*contracts.Limits)
	}{
		{"steps zero", func(l *contracts.Limits) { l.MaxSteps = 0 }},
		{"steps negative", func(l *contracts.Limits) { l.MaxSteps = -1 }},
		{"tokens zero", func(l *contracts.Limits) { l.MaxTokens = 0 }},
		{"wall clock zero", func(l *contracts.Limits) { l.MaxWallClock = 0 }},
		{"wall clock negative", func(l *contracts.Limits) { l.MaxWallClock = -time.Second }},
		{"repeat errors zero", func(l *contracts.Limits) { l.MaxRepeatErrors = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, provider := agenttest.Router(t, turnCalls(finishCall("c1")))
			g := newRig(t, router)
			_, err := g.run(t, testSpec(func(s *contracts.RoleSpec) { tc.edit(&s.Limits) }))
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("got %v, want ErrInvalidConfig", err)
			}
			if n := len(provider.Requests()); n != 0 {
				t.Errorf("%d model calls before the limits were refused", n)
			}
		})
	}
}

func TestCanonicalArgs(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{`{"a":1,"b":[1,2]}`, `{ "b": [1, 2], "a": 1 }`, true},
		{`{"a":1}`, `{"a":2}`, false},
		{`{"a":1.0}`, `{"a":1}`, false},
		{`not json`, `not json`, true},
		{`{"a":1} {}`, `{"a":1}`, false},
	}
	for _, tc := range cases {
		got := canonicalArgs(json.RawMessage(tc.a)) == canonicalArgs(json.RawMessage(tc.b))
		if got != tc.same {
			t.Errorf("canonicalArgs(%s) == canonicalArgs(%s) is %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}
