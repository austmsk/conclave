package workflow

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
)

// TestCommandApplies is the gate every command passes, over every status:
// /approve only in awaiting_approval, /cancel in every status that is not
// terminal, every other command nowhere until ticket 8b.
func TestCommandApplies(t *testing.T) {
	statuses := []contracts.FeatureRequestStatus{
		contracts.FeaturePlanning, contracts.FeatureAwaitingApproval, contracts.FeatureExecuting,
		contracts.FeatureNeedsAttention, contracts.FeatureDelivered, contracts.FeatureCancelled,
	}
	want := map[github.CommandName][]contracts.FeatureRequestStatus{
		github.CommandApprove: {contracts.FeatureAwaitingApproval},
		github.CommandCancel: {
			contracts.FeaturePlanning, contracts.FeatureAwaitingApproval,
			contracts.FeatureExecuting, contracts.FeatureNeedsAttention,
		},
	}
	for _, name := range commandNames {
		for _, st := range statuses {
			t.Run(fmt.Sprintf("%s in %s", name, st), func(t *testing.T) {
				expect := false
				for _, ok := range want[name] {
					expect = expect || ok == st
				}
				equal(t, "applies", commandApplies(name, st), expect)
			})
		}
	}
}

// TestRecentIDs is the bounded set of seen comment IDs: a repeat is seen,
// and the oldest ID is forgotten once the set is full.
func TestRecentIDs(t *testing.T) {
	var r recentIDs
	equal(t, "first sight", r.seen(1), false)
	equal(t, "repeat", r.seen(1), true)
	for id := int64(2); id <= maxRecentComments+1; id++ {
		r.seen(id)
	}
	equal(t, "size", len(r.ids), maxRecentComments)
	equal(t, "oldest forgotten", r.seen(1), false)
	equal(t, "newest kept", r.seen(maxRecentComments+1), true)
}

// TestSignals sends commands in the wrong status, twice, or from someone
// else, and checks that only the one that applies acts, exactly once.
func TestSignals(t *testing.T) {
	stranger := command(github.CommandApprove, 50)
	stranger.ActorID = ownerID + 1
	tests := []struct {
		name         string
		signals      []timedSignal
		wantApproved int
		wantMoves    []string
	}{
		{
			name: "/approve before the plan is posted is dropped, and its redelivery too",
			signals: []timedSignal{
				{0, command(github.CommandApprove, 10)},
				{time.Hour, command(github.CommandApprove, 10)},
				{2 * time.Hour, command(github.CommandCancel, 11)},
			},
			wantMoves: []string{toApproval, "awaiting_approval>cancelled"},
		},
		{
			name: "/approve from someone other than the owner is dropped",
			signals: []timedSignal{
				{time.Hour, stranger},
				{2 * time.Hour, command(github.CommandCancel, 11)},
			},
			wantMoves: []string{toApproval, "awaiting_approval>cancelled"},
		},
		{
			name: "a duplicated /approve approves once",
			signals: []timedSignal{
				{time.Hour, command(github.CommandApprove, 20)},
				{time.Hour, command(github.CommandApprove, 20)},
				{time.Hour, command(github.CommandApprove, 21)},
				{24 * time.Hour, command(github.CommandCancel, 22)},
			},
			wantApproved: 1,
			wantMoves:    []string{toApproval, toExecuting, "executing>needs_attention attempt_failed", toCancelled},
		},
		{
			name: "/approve in needs_attention is dropped",
			signals: []timedSignal{
				{time.Hour, command(github.CommandApprove, 30)},
				{12 * time.Hour, command(github.CommandApprove, 31)},
				{24 * time.Hour, command(github.CommandCancel, 32)},
			},
			wantApproved: 1,
			wantMoves:    []string{toApproval, toExecuting, "executing>needs_attention attempt_failed", toCancelled},
		},
		{
			name: "ticket 8b's commands are dropped",
			signals: []timedSignal{
				{time.Hour, command(github.CommandRetry, 40)},
				{time.Hour, command(github.CommandStatus, 41)},
				{2 * time.Hour, command(github.CommandCancel, 42)},
			},
			wantMoves: []string{toApproval, "awaiting_approval>cancelled"},
		},
		{
			name: "a redelivered /cancel after cancelled does nothing",
			signals: []timedSignal{
				{time.Hour, command(github.CommandCancel, 60)},
				{time.Hour, command(github.CommandCancel, 60)},
			},
			wantMoves: []string{toApproval, "awaiting_approval>cancelled"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld()
			w.run = returns(contracts.AttemptFailed, contracts.ReasonGateFailed)
			env := newEnv(t, w)
			for _, s := range tt.signals {
				signalAfter(env, s.after, s.sig)
			}
			env.ExecuteWorkflow(FeatureWorkflow, featureInput())
			checkCompleted(t, env)
			equal(t, "approvals", w.approved, tt.wantApproved)
			equal(t, "Approve calls", w.called("Approve"), tt.wantApproved)
			equalSlices(t, "moves", w.moves(), tt.wantMoves)
		})
	}
}

type timedSignal struct {
	after time.Duration
	sig   github.CommandSignal
}

// TestRateLimitRetriedAfterRetryAfter has the plan comment refused by a
// rate limit that names a 90-second wait: the activity is retried, not
// before the wait, and the run goes on.
func TestRateLimitRetriedAfterRetryAfter(t *testing.T) {
	w := newWorld()
	env := newEnv(t, w)
	var mu sync.Mutex
	var tries []time.Time
	env.RegisterActivityWithOptions(func(ctx context.Context, in activity.PlanCommentInput) (activity.PlanCommentResult, error) {
		mu.Lock()
		defer mu.Unlock()
		tries = append(tries, env.Now())
		if len(tries) == 1 {
			return activity.PlanCommentResult{}, activity.Temporal(&llm.RateLimitError{RetryAfter: 90 * time.Second})
		}
		return w.PostPlanComment(ctx, in)
	}, sdkRegister("PostPlanComment"))
	signalAfter(env, time.Hour, command(github.CommandCancel, 1))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	if len(tries) != 2 {
		t.Fatalf("tries = %d, want 2", len(tries))
	}
	if gap := tries[1].Sub(tries[0]); gap < 90*time.Second {
		t.Errorf("retried after %v, want at least the 90s RetryAfter", gap)
	}
	equal(t, "plan comments", w.comments, 1)
}

// TestPermanentRouterErrorNotRetried has a step fail with an error the
// router calls permanent: it is tried once and the feature request moves
// to needs_attention.
func TestPermanentRouterErrorNotRetried(t *testing.T) {
	w := newWorld()
	w.fail["PostPlanComment"] = activity.Temporal(fmt.Errorf("posting: %w", llm.ErrAuth))
	env := newEnv(t, w)
	signalAfter(env, time.Hour, command(github.CommandCancel, 1))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equal(t, "tries", w.called("PostPlanComment"), 1)
	if moves := w.moves(); len(moves) < 3 || moves[2][:len("planning>needs_attention error:")] != "planning>needs_attention error:" {
		t.Errorf("moves = %q, want a move to needs_attention with reason error", moves)
	}
}

// TestRetryableErrorIsRetried has a step fail twice with an ordinary
// error: Temporal retries it and the run goes on.
func TestRetryableErrorIsRetried(t *testing.T) {
	w := newWorld()
	env := newEnv(t, w)
	n := 0
	env.RegisterActivityWithOptions(func(ctx context.Context, in activity.PlanCommentInput) (activity.PlanCommentResult, error) {
		n++
		if n < 3 {
			return activity.PlanCommentResult{}, temporal.NewApplicationError("github 502", "transient")
		}
		return w.PostPlanComment(ctx, in)
	}, sdkRegister("PostPlanComment"))
	signalAfter(env, time.Hour, command(github.CommandCancel, 1))
	env.ExecuteWorkflow(FeatureWorkflow, featureInput())
	checkCompleted(t, env)
	equal(t, "tries", n, 3)
	equal(t, "plan comments", w.comments, 1)
}
