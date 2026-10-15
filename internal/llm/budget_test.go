package llm_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

func plannerConfig() llm.Config {
	return llm.Config{
		Models: map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
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
	return llm.ProviderResponse{ID: fmt.Sprintf("gate_%p", &ctx), StopReason: contracts.StopEndTurn, InputTokens: 5, OutputTokens: 5}, nil
}

// TestInFlightCallHoldsItsReservation: a call that is still waiting on the
// provider has already claimed its share of the budget, so a second call
// cannot spend the same headroom.
func TestInFlightCallHoldsItsReservation(t *testing.T) {
	gate := newGate()
	p := providers(fakes())
	p[llm.ProviderAnthropic] = gate
	r, err := llm.New(plannerConfig(), p, publicPlanner(), &faketest.Costs{})
	if err != nil {
		t.Fatal(err)
	}
	// One token of estimated input for "hi" plus the model's 4096 output.
	s, err := r.Scope(llm.Scope{KeyPrefix: "k", TokenBudget: 4097})
	if err != nil {
		t.Fatal(err)
	}
	req := contracts.Request{RepoID: repoID, Role: contracts.RolePlanner, Messages: userMessage("hi")}

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
	if _, err := s.Complete(context.Background(), contracts.Request{RepoID: repoID, Role: contracts.RolePlanner, Messages: userMessage("hi"), MaxTokens: 100}); err != nil {
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
				RepoID: repoID, Role: contracts.RolePlanner, Messages: userMessage("hi"), MaxTokens: 100,
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
	_, err := h.scoped.Complete(ctx, contracts.Request{RepoID: repoID, Role: contracts.RolePlanner, Messages: userMessage("hi")})
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
	costs := &faketest.Costs{}
	r, err := llm.New(embedConfig(), p, publicPlanner(), costs)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Scope(llm.Scope{KeyPrefix: "k", TokenBudget: 1000})
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
	r, err := llm.New(embedConfig(), p, publicPlanner(), &faketest.Costs{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Scope(llm.Scope{KeyPrefix: "k", TokenBudget: 10})
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
	r, err := llm.New(cfg, p, publicPlanner(), &faketest.Costs{})
	if err != nil {
		t.Fatal(err)
	}
	delete(cfg.Models, "m")
	delete(p, llm.ProviderAnthropic)
	cfg.Roles[contracts.RolePlanner].Fallbacks[0] = "gone"

	s, err := r.Scope(llm.Scope{KeyPrefix: "k", TokenBudget: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.Complete(context.Background(), contracts.Request{RepoID: repoID, Role: contracts.RolePlanner, Messages: userMessage("hi")})
	if err != nil || resp.Provenance.Model != "m2" {
		t.Fatalf("got %+v, %v", resp.Provenance, err)
	}
}
