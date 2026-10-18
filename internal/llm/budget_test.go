package llm_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

func plannerConfig() llm.Config {
	return llm.Config{
		Budgets: testBudgets,
		Models:  map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
		Roles:   map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
	}
}

func publicPlanner() llm.StaticRepos {
	return llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}
}

// gateProvider holds each call until released, so a test can have a call
// in flight while it makes another.
type gateProvider struct {
	started chan struct{}
	release chan struct{}
	ids     atomic.Int64
}

func newGate() *gateProvider {
	return &gateProvider{started: make(chan struct{}, 8), release: make(chan struct{}, 8)}
}

func (g *gateProvider) Complete(ctx context.Context, _ llm.ProviderRequest) (llm.ProviderResponse, error) {
	g.started <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return llm.ProviderResponse{}, ctx.Err()
	}
	return llm.ProviderResponse{ID: fmt.Sprintf("gate_%d", g.ids.Add(1)), StopReason: contracts.StopEndTurn, InputTokens: 5, OutputTokens: 5}, nil
}

// TestInFlightCallHoldsItsReservation: a call that is still waiting on the
// provider has already claimed its share of the budget, so a second call
// cannot spend the same headroom.
func TestInFlightCallHoldsItsReservation(t *testing.T) {
	gate := newGate()
	p := providers(fakes())
	p[llm.ProviderAnthropic] = gate
	r, err := llm.New(plannerConfig(), p, publicPlanner(), newCosts())
	if err != nil {
		t.Fatal(err)
	}
	// One token of estimated input for "hi" plus the model's 4096 output.
	s, err := r.Scope(context.Background(), llm.Scope{RepoID: repoID, FeatureRequestID: featureID, AttemptID: attemptID, KeyPrefix: "k", TokenBudget: 4097})
	if err != nil {
		t.Fatal(err)
	}
	req := contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi")}

	first := make(chan error, 1)
	go func() {
		_, err := s.Complete(context.Background(), req)
		first <- err
	}()
	<-gate.started
	// Bounded, so a regression that lets this call through to the gate
	// fails instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.Complete(ctx, req); !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Fatalf("second call while first in flight: got %v, want ErrBudgetExceeded", err)
	}
	gate.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := s.TokensUsed(); got != 10 {
		t.Fatalf("TokensUsed = %d, want 10", got)
	}

	// The first call settled at 10 tokens, so the headroom is back.
	gate.release <- struct{}{}
	if _, err := s.Complete(context.Background(), contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi"), MaxTokens: 100}); err != nil {
		t.Fatalf("call after settling: %v", err)
	}
}

// TestConcurrentCallsStayInBudget: seventeen concurrent calls each reserve
// 101 tokens against a budget of sixteen calls. Exactly one is refused and
// the budget is never exceeded, whatever order they run in.
func TestConcurrentCallsStayInBudget(t *testing.T) {
	const fits = 16
	turns := make([]faketest.Turn, fits+1)
	for i := range turns {
		turns[i] = endTurn(1, 100)
	}
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(turns...)
	h := newHarness(t, plannerConfig(), f, publicPlanner(), fits*101)

	var wg sync.WaitGroup
	errs := make(chan error, fits+1)
	for range fits + 1 {
		wg.Go(func() {
			_, err := h.scoped.Complete(context.Background(), contracts.Request{
				RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi"), MaxTokens: 100,
			})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	refused := 0
	for err := range errs {
		switch {
		case errors.Is(err, llm.ErrBudgetExceeded):
			refused++
		case err != nil:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if refused != 1 || h.scoped.TokensUsed() != fits*101 || len(h.costs.Events()) != fits {
		t.Fatalf("refused %d, used %d, %d events", refused, h.scoped.TokensUsed(), len(h.costs.Events()))
	}
}

// TestRateLimitHoldsRequests: with one request a minute, a second call
// waits for the limiter and gives up when its context does, without
// reaching the provider.
func TestRateLimitHoldsRequests(t *testing.T) {
	cfg := plannerConfig()
	cfg.RateLimit = llm.RateLimit{RequestsPerMinute: 1}
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(endTurn(1, 1), endTurn(1, 1))
	h := newHarness(t, cfg, f, publicPlanner(), 10_000)
	if _, err := complete(h, contracts.RolePlanner); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := h.scoped.Complete(ctx, contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want DeadlineExceeded", err)
	}
	if n := len(f[llm.ProviderAnthropic].Requests()); n != 1 {
		t.Fatalf("provider received %d requests, want 1", n)
	}
	if got := h.scoped.TokensUsed(); got != 2 {
		t.Fatalf("TokensUsed = %d: a refused call must release its reservation", got)
	}
}

func embedConfig() llm.Config {
	cfg := plannerConfig()
	cfg.Models["e"] = model(llm.ProviderOllama)
	cfg.Embedding = &llm.EmbeddingRoute{Model: "e"}
	return cfg
}

func TestEmbedChargesBudgetAndRecordsCost(t *testing.T) {
	emb := &stubEmbedder{Provider: faketest.New()}
	p := providers(fakes())
	p[llm.ProviderOllama] = emb
	costs := newCosts()
	r, err := llm.New(embedConfig(), p, publicPlanner(), costs)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Scope(context.Background(), llm.Scope{RepoID: repoID, FeatureRequestID: featureID, AttemptID: attemptID, KeyPrefix: "k", TokenBudget: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Embed(context.Background(), repoID, []string{"func main() {}"}); err != nil {
		t.Fatal(err)
	}
	evs := costs.Events()
	if len(evs) != 1 || evs[0].Role != "embedding" || evs[0].Model != "e" || evs[0].InputTokens != 100 ||
		evs[0].IdempotencyKey != "k/ollama/emb_1" {
		t.Fatalf("events %+v", evs)
	}
	if got := s.TokensUsed(); got != 100 {
		t.Fatalf("TokensUsed = %d, want 100", got)
	}
}

func TestEmbedRefusedOverBudget(t *testing.T) {
	emb := &stubEmbedder{Provider: faketest.New()}
	p := providers(fakes())
	p[llm.ProviderOllama] = emb
	r, err := llm.New(embedConfig(), p, publicPlanner(), newCosts())
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Scope(context.Background(), llm.Scope{RepoID: repoID, FeatureRequestID: featureID, AttemptID: attemptID, KeyPrefix: "k", TokenBudget: 10})
	if err != nil {
		t.Fatal(err)
	}
	long := string(make([]byte, 300))
	if _, err := s.Embed(context.Background(), repoID, []string{long}); !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Fatalf("got %v, want ErrBudgetExceeded", err)
	}
	if emb.calls != 0 {
		t.Fatal("embedder called over budget")
	}
}

func TestEmbedNeedsAnEmbeddingModel(t *testing.T) {
	cases := []struct {
		name string
		cfg  llm.Config
	}{
		{"none configured", plannerConfig()},
		{"provider cannot embed", embedConfig()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.cfg, fakes(), publicPlanner(), 1000)
			if _, err := h.scoped.Embed(context.Background(), repoID, []string{"x"}); !errors.Is(err, llm.ErrNoEmbeddingModel) {
				t.Fatalf("got %v, want ErrNoEmbeddingModel", err)
			}
		})
	}
}

// TestRouterKeepsItsOwnConfig: changing the maps passed to New afterwards
// cannot remove a validated model, route, or adapter.
func TestRouterKeepsItsOwnConfig(t *testing.T) {
	cfg := plannerConfig()
	cfg.Roles[contracts.RolePlanner] = llm.RoleRoute{Model: "m", Fallbacks: []string{"m2"}}
	cfg.Models["m2"] = model(llm.ProviderAnthropic)
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(faketest.Turn{Error: "unavailable"}, endTurn(1, 1))
	p := providers(f)
	r, err := llm.New(cfg, p, publicPlanner(), newCosts())
	if err != nil {
		t.Fatal(err)
	}
	delete(cfg.Models, "m")
	delete(p, llm.ProviderAnthropic)
	cfg.Roles[contracts.RolePlanner].Fallbacks[0] = "gone"

	s, err := r.Scope(context.Background(), llm.Scope{RepoID: repoID, FeatureRequestID: featureID, AttemptID: attemptID, KeyPrefix: "k", TokenBudget: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.Complete(context.Background(), contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi")})
	if err != nil || resp.Provenance.Model != "m2" {
		t.Fatalf("got %+v, %v", resp.Provenance, err)
	}
}

// TestRejectedReplyIsCharged: the provider bills a reply that the adapter
// then refuses, a refusal say. The tokens are charged to the budget and
// recorded, so a model that refuses on every retry still runs the budget
// out instead of spending without limit. Each reply has its own ID, as a
// provider's do; the ledger counts a repeated ID once.
func TestRejectedReplyIsCharged(t *testing.T) {
	rejected := func(id string) faketest.Turn {
		return faketest.Turn{Error: "rejected", Response: faketest.Response{ID: id, InputTokens: 300, OutputTokens: 200}}
	}
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(rejected("msg_refused"), rejected("msg_refused_2"), rejected("msg_refused_3"))
	h := newHarness(t, plannerConfig(), f, publicPlanner(), 1000)
	req := contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi"), MaxTokens: 200}

	if _, err := h.scoped.Complete(context.Background(), req); !errors.Is(err, faketest.ErrRejected) {
		t.Fatalf("first call: got %v, want ErrRejected", err)
	}
	if got := h.scoped.TokensUsed(); got != 500 {
		t.Fatalf("TokensUsed = %d, want 500", got)
	}
	evs := h.costs.Events()
	if len(evs) != 1 || evs[0].IdempotencyKey != "wf/act/anthropic/msg_refused" || evs[0].InputTokens != 300 || evs[0].OutputTokens != 200 {
		t.Fatalf("events %+v", evs)
	}
	if _, err := h.scoped.Complete(context.Background(), req); !errors.Is(err, faketest.ErrRejected) {
		t.Fatalf("second call: got %v, want ErrRejected", err)
	}
	if _, err := h.scoped.Complete(context.Background(), req); !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Fatalf("third call: got %v, want ErrBudgetExceeded", err)
	}
	if f[llm.ProviderAnthropic].Remaining() != 1 {
		t.Fatal("the call over budget reached the provider")
	}
}

// TestRejectedReplyRecordingFailure: when a billed reply cannot be recorded,
// the ledger's error is what the caller sees, so it is not lost behind the
// provider's.
func TestRejectedReplyRecordingFailure(t *testing.T) {
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(faketest.Turn{Error: "rejected", Response: faketest.Response{InputTokens: 3, OutputTokens: 2}})
	boom := errors.New("database down")
	costs := newCosts()
	costs.SettleErr = boom
	r, err := llm.New(plannerConfig(), providers(f), publicPlanner(), costs)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Scope(context.Background(), llm.Scope{RepoID: repoID, FeatureRequestID: featureID, AttemptID: attemptID, KeyPrefix: "k", TokenBudget: 1000})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Complete(context.Background(), contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi")})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the ledger's error", err)
	}
}

// TestSecondScopeSeesInFlightCall attempts the overspend 8a closes: an
// activity abandoned after a heartbeat timeout still has a call in flight
// when its retry opens a second scope for the same attempt. The in-flight
// call's reservation counts against the attempt, so the retry is refused
// before reaching the provider, rather than spending the budget a second
// time.
func TestSecondScopeSeesInFlightCall(t *testing.T) {
	gate := newGate()
	p := providers(fakes())
	p[llm.ProviderAnthropic] = gate
	costs := newCosts()
	r, err := llm.New(plannerConfig(), p, publicPlanner(), costs)
	if err != nil {
		t.Fatal(err)
	}
	// One token of estimated input for "hi" plus the model's 4096 output.
	scope := llm.Scope{RepoID: repoID, FeatureRequestID: featureID, AttemptID: attemptID, KeyPrefix: "wf/act-1", TokenBudget: 4097}
	abandoned, err := r.Scope(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	scope.KeyPrefix = "wf/act-2"
	retry, err := r.Scope(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	req := plannerRequest(0)

	first := make(chan error, 1)
	go func() {
		_, err := abandoned.Complete(context.Background(), req)
		first <- err
	}()
	<-gate.started
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = retry.Complete(ctx, req)
	assertBudgetKind(t, err, llm.BudgetAttempt)
	gate.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("in-flight call: %v", err)
	}
	if n := costs.Reservations(); n != 0 {
		t.Fatalf("%d reservations after every call settled, want 0", n)
	}
	if evs := costs.Events(); len(evs) != 1 || evs[0].AttemptID != attemptID || evs[0].FeatureRequestID != featureID {
		t.Fatalf("events %+v, want the in-flight call's alone, attributed", evs)
	}
}

// TestCapRefusals: a call that would pass any of the three caps is refused
// before it reaches a provider, with a BudgetError naming the cap. The
// first call in each case fits and spends; the second does not fit.
func TestCapRefusals(t *testing.T) {
	// The test model prices input at $3 and output at $15 per million
	// tokens, so a call of 1 input and 4096 output tokens claims about
	// $0.0615, and a reply of 1000 and 1000 tokens costs $0.018.
	cases := []struct {
		name    string
		budget  int
		budgets llm.Budgets
		model   func(llm.ModelConfig) llm.ModelConfig
		calls   int
		want    llm.BudgetKind
	}{
		{"attempt", 2000, testBudgets, nil, 1, llm.BudgetAttempt},
		{"feature request", 1 << 30, llm.Budgets{FeatureRequestTokens: 2000, MonthlyUSD: 1e9}, nil, 1, llm.BudgetFeatureRequest},
		{"month", 1 << 30, llm.Budgets{FeatureRequestTokens: 1 << 40, MonthlyUSD: 0.07}, nil, 1, llm.BudgetMonth},
		// The claim prices input at the dearest rate it could be billed
		// at: an input estimate billed as cache writes costs more than
		// plain input.
		{"month at the cache write price", 1 << 30, llm.Budgets{FeatureRequestTokens: 1 << 40, MonthlyUSD: 0.07},
			func(m llm.ModelConfig) llm.ModelConfig { m.CacheWriteUSDPerMTok = 1e5; return m }, 0, llm.BudgetMonth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := plannerConfig()
			cfg.Budgets = tc.budgets
			if tc.model != nil {
				cfg.Models["m"] = tc.model(cfg.Models["m"])
			}
			f := fakes()
			f[llm.ProviderAnthropic] = faketest.New(endTurn(1000, 1000), endTurn(1, 1))
			h := newHarness(t, cfg, f, publicPlanner(), tc.budget)
			for i := range tc.calls {
				if _, err := h.scoped.Complete(context.Background(), plannerRequest(0)); err != nil {
					t.Fatalf("call %d: %v", i+1, err)
				}
			}
			_, err := h.scoped.Complete(context.Background(), plannerRequest(0))
			assertBudgetKind(t, err, tc.want)
			if sent := 2 - f[llm.ProviderAnthropic].Remaining(); sent != tc.calls {
				t.Fatalf("provider received %d requests, want %d", sent, tc.calls)
			}
			if n := h.costs.Reservations(); n != 0 {
				t.Fatalf("%d reservations left by a refusal, want 0", n)
			}
		})
	}
}

// TestFailedCallReleasesReservation: a call that fails without billing
// leaves neither an event nor a reservation behind, so its claim stops
// counting at once rather than when it expires.
func TestFailedCallReleasesReservation(t *testing.T) {
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(faketest.Turn{Error: "unavailable"})
	h := newHarness(t, plannerConfig(), f, publicPlanner(), 10_000)
	if _, err := h.scoped.Complete(context.Background(), plannerRequest(0)); !errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	if n, evs := h.costs.Reservations(), len(h.costs.Events()); n != 0 || evs != 0 {
		t.Fatalf("%d reservations and %d events after an unbilled failure, want none", n, evs)
	}
}

// billedOnCancel replies only when the caller gives up, and bills the call
// anyway, as a provider does for a generation the client stopped waiting
// for.
type billedOnCancel struct{ started chan struct{} }

func (b billedOnCancel) Complete(ctx context.Context, _ llm.ProviderRequest) (llm.ProviderResponse, error) {
	b.started <- struct{}{}
	<-ctx.Done()
	return llm.ProviderResponse{ID: "msg_cancelled", InputTokens: 50, OutputTokens: 50}, ctx.Err()
}

// TestCancelledCallIsStillRecorded: a call billed after its caller's
// context was cancelled is still recorded and its reservation settled.
// Recording with the cancelled context would fail, and the spend would stop
// counting once the reservation expired.
func TestCancelledCallIsStillRecorded(t *testing.T) {
	p := providers(fakes())
	started := make(chan struct{}, 1)
	p[llm.ProviderAnthropic] = billedOnCancel{started: started}
	costs := newCosts()
	r, err := llm.New(plannerConfig(), p, publicPlanner(), costs)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Scope(context.Background(), llm.Scope{RepoID: repoID, FeatureRequestID: featureID, AttemptID: attemptID, KeyPrefix: "k", TokenBudget: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := s.Complete(ctx, plannerRequest(100)); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want Canceled", err)
	}
	evs := costs.Events()
	if len(evs) != 1 || evs[0].InputTokens+evs[0].OutputTokens != 100 || costs.Reservations() != 0 {
		t.Fatalf("events %+v and %d reservations, want the billed call recorded and nothing held", evs, costs.Reservations())
	}
}

// deadlineLedger records the deadline each Settle was given.
type deadlineLedger struct {
	*faketest.Costs
	mu        sync.Mutex
	deadlines []time.Time
}

func (d *deadlineLedger) Settle(ctx context.Context, id string, ev contracts.CostEvent) error {
	deadline, _ := ctx.Deadline()
	d.mu.Lock()
	d.deadlines = append(d.deadlines, deadline)
	d.mu.Unlock()
	return d.Costs.Settle(ctx, id, ev)
}

// TestSettleKeepsLongerDeadline: recording a billed reply gets at least
// the caller's own deadline. A fixed short timeout would cut it down, and a
// database slow for longer than that would lose the call from the ledger.
// A caller with less time left still gets the minimum.
func TestSettleKeepsLongerDeadline(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		atLeast time.Duration
		atMost  time.Duration
	}{
		{"long caller deadline", time.Hour, 59 * time.Minute, time.Hour},
		{"short caller deadline", time.Second, 25 * time.Second, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fakes()
			f[llm.ProviderAnthropic] = faketest.New(endTurn(1, 1))
			ledger := &deadlineLedger{Costs: newCosts()}
			r, err := llm.New(plannerConfig(), providers(f), publicPlanner(), ledger)
			if err != nil {
				t.Fatal(err)
			}
			s, err := r.Scope(context.Background(), llm.Scope{RepoID: repoID, FeatureRequestID: featureID, AttemptID: attemptID, KeyPrefix: "k", TokenBudget: 10_000})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			if _, err := s.Complete(ctx, plannerRequest(100)); err != nil {
				t.Fatal(err)
			}
			if len(ledger.deadlines) != 1 {
				t.Fatalf("%d settles, want 1", len(ledger.deadlines))
			}
			left := time.Until(ledger.deadlines[0])
			if left < tc.atLeast || left > tc.atMost {
				t.Fatalf("settle had %s left, want between %s and %s", left, tc.atLeast, tc.atMost)
			}
		})
	}
}
