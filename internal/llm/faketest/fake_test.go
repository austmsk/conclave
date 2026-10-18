package faketest_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

func toolTurn(ids ...string) faketest.Turn {
	var calls []contracts.ToolCall
	for _, id := range ids {
		calls = append(calls, contracts.ToolCall{ID: id, Name: contracts.ToolReadFile})
	}
	return faketest.Turn{Response: faketest.Response{ToolCalls: calls, StopReason: contracts.StopToolUse}}
}

func endTurn() faketest.Turn {
	return faketest.Turn{Response: faketest.Response{Text: "done", StopReason: contracts.StopEndTurn}}
}

func user(text string) contracts.Message {
	return contracts.Message{Role: contracts.MessageUser, Text: text}
}

func asked(ids ...string) contracts.Message {
	m := contracts.Message{Role: contracts.MessageAssistant}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, contracts.ToolCall{ID: id, Name: contracts.ToolReadFile})
	}
	return m
}

func answered(ids ...string) contracts.Message {
	m := contracts.Message{Role: contracts.MessageUser}
	for _, id := range ids {
		m.ToolResults = append(m.ToolResults, contracts.ToolResult{CallID: id})
	}
	return m
}

// TestSecondRequestMustAnswerToolCalls: the fake refuses a follow-up that
// does not carry a result for every call it made, the kind of loop or
// adapter bug that otherwise looks like odd model behaviour (RL-6).
func TestSecondRequestMustAnswerToolCalls(t *testing.T) {
	cases := []struct {
		name string
		msgs []contracts.Message
		ok   bool
	}{
		{"both answered", []contracts.Message{user("go"), asked("a", "b"), answered("a", "b")}, true},
		{"answered out of order", []contracts.Message{user("go"), asked("a", "b"), answered("b", "a")}, true},
		{"one result dropped", []contracts.Message{user("go"), asked("a", "b"), answered("a")}, false},
		{"wrong call ID", []contracts.Message{user("go"), asked("a", "b"), answered("a", "c")}, false},
		{"assistant turn missing", []contracts.Message{user("go"), answered("a", "b")}, false},
		{"assistant turn altered", []contracts.Message{user("go"), asked("a"), answered("a", "b")}, false},
		{"no tool round", []contracts.Message{user("go")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := faketest.New(toolTurn("a", "b"), endTurn())
			ctx := context.Background()
			if _, err := p.Complete(ctx, llm.ProviderRequest{Messages: []contracts.Message{user("go")}}); err != nil {
				t.Fatal(err)
			}
			_, err := p.Complete(ctx, llm.ProviderRequest{Messages: tc.msgs})
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if !tc.ok && !errors.Is(err, faketest.ErrUnexpectedRequest) {
				t.Fatalf("got %v, want ErrUnexpectedRequest", err)
			}
		})
	}
}

func TestExpectations(t *testing.T) {
	cases := []struct {
		name   string
		expect faketest.Expect
		req    llm.ProviderRequest
		ok     bool
	}{
		{"model matches", faketest.Expect{Model: "m"}, llm.ProviderRequest{Model: "m"}, true},
		{"model differs", faketest.Expect{Model: "m"}, llm.ProviderRequest{Model: "n"}, false},
		{"message count differs", faketest.Expect{Messages: 2}, llm.ProviderRequest{Messages: []contracts.Message{user("x")}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn := endTurn()
			turn.Expect = tc.expect
			_, err := faketest.New(turn).Complete(context.Background(), tc.req)
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestExhausted(t *testing.T) {
	p := faketest.New(endTurn())
	ctx := context.Background()
	if _, err := p.Complete(ctx, llm.ProviderRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(ctx, llm.ProviderRequest{}); !errors.Is(err, faketest.ErrExhausted) {
		t.Fatalf("got %v, want ErrExhausted", err)
	}
}

func TestScriptedErrors(t *testing.T) {
	cases := []struct {
		script string
		want   error
	}{
		{"unavailable", llm.ErrUnavailable},
		{"rate_limited", llm.ErrRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.script, func(t *testing.T) {
			_, err := faketest.New(faketest.Turn{Error: tc.script}).Complete(context.Background(), llm.ProviderRequest{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := t.TempDir() + "/bad.json"
	if err := os.WriteFile(path, []byte(`{"turns": [{"respons": {}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := faketest.Load(path); err == nil {
		t.Fatal("Load accepted a fixture with an unknown field")
	}
}

// reserveFor reserves a claim of tokens for an attempt of feature "f".
func reserveFor(t *testing.T, c *faketest.Costs, attempt string, tokens int) string {
	t.Helper()
	id, err := c.Reserve(context.Background(), attempt, "f", time.Minute, func(llm.Spend) (llm.Claim, error) {
		return llm.Claim{Tokens: tokens}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCostsIgnoresRepeatedKey(t *testing.T) {
	c := &faketest.Costs{}
	ctx := context.Background()
	id := reserveFor(t, c, "a", 1)
	for range 2 {
		if err := c.Settle(ctx, id, contracts.CostEvent{IdempotencyKey: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	if n, held := len(c.Events()), c.Reservations(); n != 1 || held != 0 {
		t.Fatalf("%d events and %d reservations, want 1 and 0", n, held)
	}
}

// TestCostsSpendCountsReservations: Reserve sees settled events and live
// reservations, each against the caps they belong to.
func TestCostsSpendCountsReservations(t *testing.T) {
	c := &faketest.Costs{}
	ctx := context.Background()
	settled := reserveFor(t, c, "attempt-1", 100)
	if err := c.Settle(ctx, settled, contracts.CostEvent{IdempotencyKey: "a", AttemptID: "attempt-1", FeatureRequestID: "f", InputTokens: 10, OutputTokens: 5, CostUSD: 1}); err != nil {
		t.Fatal(err)
	}
	reserveFor(t, c, "attempt-1", 20)
	reserveFor(t, c, "attempt-2", 40)
	released := reserveFor(t, c, "attempt-1", 1000)
	if err := c.Release(ctx, released); err != nil {
		t.Fatal(err)
	}

	var got llm.Spend
	if _, err := c.Reserve(ctx, "attempt-1", "f", time.Minute, func(sp llm.Spend) (llm.Claim, error) {
		got = sp
		return llm.Claim{}, errors.New("refused")
	}); err == nil {
		t.Fatal("decide's error was not returned")
	}
	want := llm.Spend{AttemptTokens: 35, FeatureRequestTokens: 75, MonthUSD: 1}
	if got != want {
		t.Fatalf("spend %+v, want %+v", got, want)
	}
	if c.Reservations() != 2 {
		t.Fatalf("%d reservations, want 2: a refused Reserve must not hold one", c.Reservations())
	}
}

func TestCostsRefusesUnknownReservation(t *testing.T) {
	c := &faketest.Costs{}
	if err := c.Release(context.Background(), "never-issued"); !errors.Is(err, faketest.ErrUnknownReservation) {
		t.Fatalf("got %v, want ErrUnknownReservation", err)
	}
}

// TestRejectedTurnReportsUsage: a "rejected" turn fails and still carries
// the billed ID and tokens, as a real adapter's rejection does.
func TestRejectedTurnReportsUsage(t *testing.T) {
	p := faketest.New(faketest.Turn{Error: "rejected", Response: faketest.Response{ID: "msg_x", Text: "dropped", InputTokens: 7, OutputTokens: 3}})
	resp, err := p.Complete(context.Background(), llm.ProviderRequest{Model: "m"})
	if !errors.Is(err, faketest.ErrRejected) {
		t.Fatalf("got %v, want ErrRejected", err)
	}
	if resp.ID != "msg_x" || resp.InputTokens != 7 || resp.OutputTokens != 3 || resp.Text != "" {
		t.Fatalf("response %+v", resp)
	}
}
