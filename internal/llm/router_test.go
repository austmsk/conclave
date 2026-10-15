package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

const repoID = "repo-1"

var allTiers = []contracts.SensitivityTier{
	contracts.TierPublic, contracts.TierPrivate, contracts.TierSensitive, contracts.TierLocalOnly,
}

var allRoles = []contracts.Role{
	contracts.RoleImplementer, contracts.RoleTestWriter, contracts.RoleBrief, contracts.RolePlanner,
	contracts.RoleJudge, contracts.RolePRWriter, contracts.RoleDiscussion, contracts.RoleConflictResolver,
}

var allProviders = []string{llm.ProviderAnthropic, llm.ProviderOpenAI, llm.ProviderGoogle, llm.ProviderOllama}

// sensitiveApproved is the owner-approved list used for sensitive fixtures.
var sensitiveApproved = []string{llm.ProviderAnthropic}

// tierAllows is written out independently of the router's own table, from
// security-and-threat-model.md, so a mistake in one shows up as a
// disagreement with the other.
var tierAllows = map[contracts.SensitivityTier]map[string]bool{
	contracts.TierPublic:    {"anthropic": true, "openai": true, "google": true, "ollama": true},
	contracts.TierPrivate:   {"anthropic": true, "openai": true, "google": true, "ollama": false},
	contracts.TierSensitive: {"anthropic": true, "openai": false, "google": false, "ollama": false},
	contracts.TierLocalOnly: {"anthropic": false, "openai": false, "google": false, "ollama": true},
}

func model(provider string) llm.ModelConfig {
	return llm.ModelConfig{Provider: provider, InputUSDPerMTok: 3, OutputUSDPerMTok: 15, MaxOutputTokens: 4096}
}

func policy(tier contracts.SensitivityTier, roles ...contracts.Role) llm.RepoPolicy {
	return llm.RepoPolicy{Tier: tier, Roles: roles, SensitiveProviders: sensitiveApproved}
}

func userMessage(text string) []contracts.Message {
	return []contracts.Message{{Role: contracts.MessageUser, Text: text}}
}

func endTurn(input, output int) faketest.Turn {
	return faketest.Turn{Response: faketest.Response{Text: "ok", StopReason: contracts.StopEndTurn, InputTokens: input, OutputTokens: output}}
}

// fakes returns one empty fake per known provider.
func fakes() map[string]*faketest.Provider {
	m := map[string]*faketest.Provider{}
	for _, p := range allProviders {
		m[p] = faketest.New()
	}
	return m
}

func providers(f map[string]*faketest.Provider) map[string]llm.Provider {
	m := map[string]llm.Provider{}
	for name, p := range f {
		m[name] = p
	}
	return m
}

type harness struct {
	scoped *llm.Scoped
	costs  *faketest.Costs
}

func newHarness(t *testing.T, cfg llm.Config, f map[string]*faketest.Provider, repos llm.StaticRepos, budget int) harness {
	t.Helper()
	costs := &faketest.Costs{}
	r, err := llm.New(cfg, providers(f), repos, costs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s, err := r.Scope(llm.Scope{AttemptID: "attempt-1", KeyPrefix: "wf/act", TokenBudget: budget})
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	return harness{scoped: s, costs: costs}
}

func complete(h harness, role contracts.Role) (contracts.Response, error) {
	return h.scoped.Complete(context.Background(), contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: role, Messages: userMessage("hi")})
}

// TestPolicyMatrix sends one request for every tier, role, and provider. A
// disallowed provider must fail with ErrTierDenied and must never see the
// request.
func TestPolicyMatrix(t *testing.T) {
	for _, tier := range allTiers {
		for _, role := range allRoles {
			for _, provider := range allProviders {
				t.Run(fmt.Sprintf("%s/%s/%s", tier, role, provider), func(t *testing.T) {
					policyCase(t, tier, role, provider)
				})
			}
		}
	}
}

func policyCase(t *testing.T, tier contracts.SensitivityTier, role contracts.Role, provider string) {
	f := fakes()
	f[provider] = faketest.New(endTurn(10, 5))
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": model(provider)},
		Roles:  map[contracts.Role]llm.RoleRoute{role: {Model: "m"}},
	}
	h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(tier, role)}, 100000)

	resp, err := complete(h, role)
	allowed := tierAllows[tier][provider]
	assertTierOutcome(t, allowed, err)
	sent := len(f[provider].Requests())
	if allowed {
		if sent != 1 || resp.Provenance.Provider != provider || resp.Provenance.PromptVersion != "test-v1" {
			t.Fatalf("sent %d requests, provenance %+v", sent, resp.Provenance)
		}
		return
	}
	if sent != 0 {
		t.Fatalf("disallowed provider received %d requests", sent)
	}
	if n := len(h.costs.Events()); n != 0 {
		t.Fatalf("denied request recorded %d cost events", n)
	}
}

func assertTierOutcome(t *testing.T, allowed bool, err error) {
	t.Helper()
	if allowed && err != nil {
		t.Fatalf("allowed request failed: %v", err)
	}
	if !allowed && !errors.Is(err, llm.ErrTierDenied) {
		t.Fatalf("got %v, want ErrTierDenied", err)
	}
}

// TestRoleAllowlistMatrix: a role absent from the repository's allowlist is
// denied for every tier, before any provider is called.
func TestRoleAllowlistMatrix(t *testing.T) {
	for _, tier := range allTiers {
		for _, role := range allRoles {
			t.Run(fmt.Sprintf("%s/%s", tier, role), func(t *testing.T) {
				other := contracts.RoleDiscussion
				if role == other {
					other = contracts.RoleBrief
				}
				f := fakes()
				f[llm.ProviderOllama] = faketest.New(endTurn(1, 1))
				cfg := llm.Config{
					Models: map[string]llm.ModelConfig{"m": model(llm.ProviderOllama)},
					Roles:  map[contracts.Role]llm.RoleRoute{role: {Model: "m"}},
				}
				h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(tier, other)}, 100000)
				if _, err := complete(h, role); !errors.Is(err, llm.ErrRoleDenied) {
					t.Fatalf("got %v, want ErrRoleDenied", err)
				}
				if n := len(f[llm.ProviderOllama].Requests()); n != 0 {
					t.Fatalf("provider received %d requests", n)
				}
			})
		}
	}
}

// TestRepoPolicyFailsClosed: a repository with no tier, an unknown tier, no
// role allowlist, or no configuration at all cannot make a request.
func TestRepoPolicyFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		repos  llm.StaticRepos
		repoID string
		want   error
	}{
		{"no tier", llm.StaticRepos{repoID: {Roles: []contracts.Role{contracts.RolePlanner}}}, repoID, llm.ErrNoTier},
		{"unknown tier", llm.StaticRepos{repoID: policy("internal", contracts.RolePlanner)}, repoID, llm.ErrUnknownTier},
		{"no role allowlist", llm.StaticRepos{repoID: {Tier: contracts.TierPublic}}, repoID, llm.ErrNoRoleAllowlist},
		{"unconfigured repository", llm.StaticRepos{}, repoID, llm.ErrUnknownRepo},
		{"empty repository ID", llm.StaticRepos{"": policy(contracts.TierPublic, contracts.RolePlanner)}, "", llm.ErrUnknownRepo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fakes()
			f[llm.ProviderOllama] = faketest.New(endTurn(1, 1))
			cfg := llm.Config{
				Models: map[string]llm.ModelConfig{"m": model(llm.ProviderOllama)},
				Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
			}
			h := newHarness(t, cfg, f, tc.repos, 100000)
			_, err := h.scoped.Complete(context.Background(), contracts.Request{RepoID: tc.repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi")})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Complete: got %v, want %v", err, tc.want)
			}
			if _, err := h.scoped.Embed(context.Background(), tc.repoID, []string{"x"}); !errors.Is(err, tc.want) {
				t.Fatalf("Embed: got %v, want %v", err, tc.want)
			}
			if n := len(f[llm.ProviderOllama].Requests()); n != 0 {
				t.Fatalf("provider received %d requests", n)
			}
		})
	}
}

// primaryFor is a provider every tier's fixtures allow for the primary.
func primaryFor(tier contracts.SensitivityTier) string {
	if tier == contracts.TierLocalOnly {
		return llm.ProviderOllama
	}
	return llm.ProviderAnthropic
}

// TestFallbackMatrix: the primary is unavailable, and the fallback is on
// each provider in turn. A fallback the tier disallows fails the request
// rather than proceeding, and never receives it.
func TestFallbackMatrix(t *testing.T) {
	for _, tier := range allTiers {
		for _, fb := range allProviders {
			t.Run(fmt.Sprintf("%s/%s", tier, fb), func(t *testing.T) {
				primary := primaryFor(tier)
				f := fakes()
				unavailable := faketest.Turn{Error: "unavailable"}
				if fb == primary {
					f[primary] = faketest.New(unavailable, endTurn(10, 5))
				} else {
					f[primary] = faketest.New(unavailable)
					f[fb] = faketest.New(endTurn(10, 5))
				}
				cfg := llm.Config{
					Models: map[string]llm.ModelConfig{"primary": model(primary), "fallback": model(fb)},
					Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "primary", Fallbacks: []string{"fallback"}}},
				}
				h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(tier, contracts.RolePlanner)}, 100000)

				resp, err := complete(h, contracts.RolePlanner)
				allowed := tierAllows[tier][fb]
				assertTierOutcome(t, allowed, err)
				if allowed && resp.Provenance.Model != "fallback" {
					t.Fatalf("served by %q, want fallback", resp.Provenance.Model)
				}
				if !allowed && fb != primary && len(f[fb].Requests()) != 0 {
					t.Fatal("disallowed fallback received the request")
				}
			})
		}
	}
}

// TestDisallowedFallbackIsNotSkipped: when a disallowed fallback sits before
// an allowed one, the request fails at the disallowed one instead of
// quietly moving further down the chain.
func TestDisallowedFallbackIsNotSkipped(t *testing.T) {
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(faketest.Turn{Error: "unavailable"})
	f[llm.ProviderOpenAI] = faketest.New(endTurn(1, 1))
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{
			"primary": model(llm.ProviderAnthropic), "local": model(llm.ProviderOllama), "other": model(llm.ProviderOpenAI),
		},
		Roles: map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "primary", Fallbacks: []string{"local", "other"}}},
	}
	h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(contracts.TierPrivate, contracts.RolePlanner)}, 100000)
	if _, err := complete(h, contracts.RolePlanner); !errors.Is(err, llm.ErrTierDenied) {
		t.Fatalf("got %v, want ErrTierDenied", err)
	}
	if len(f[llm.ProviderOpenAI].Requests()) != 0 || len(f[llm.ProviderOllama].Requests()) != 0 {
		t.Fatal("a fallback after the denied one was called")
	}
}

// TestFallbackOnlyOnUnavailable: other errors are returned, not retried on
// another model.
func TestFallbackOnlyOnUnavailable(t *testing.T) {
	cases := []struct {
		name      string
		primary   faketest.Turn
		want      error
		fallbacks int
	}{
		{"rate limited is returned", faketest.Turn{Error: "rate_limited"}, llm.ErrRateLimited, 0},
		{"unavailable falls back", faketest.Turn{Error: "unavailable"}, nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fakes()
			f[llm.ProviderAnthropic] = faketest.New(tc.primary)
			f[llm.ProviderOpenAI] = faketest.New(endTurn(1, 1))
			cfg := llm.Config{
				Models: map[string]llm.ModelConfig{"a": model(llm.ProviderAnthropic), "b": model(llm.ProviderOpenAI)},
				Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "a", Fallbacks: []string{"b"}}},
			}
			h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}, 100000)
			_, err := complete(h, contracts.RolePlanner)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if n := len(f[llm.ProviderOpenAI].Requests()); n != tc.fallbacks {
				t.Fatalf("fallback received %d requests, want %d", n, tc.fallbacks)
			}
		})
	}
}

func TestEveryModelUnavailable(t *testing.T) {
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(faketest.Turn{Error: "unavailable"}, faketest.Turn{Error: "unavailable"})
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"a": model(llm.ProviderAnthropic), "b": model(llm.ProviderAnthropic)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "a", Fallbacks: []string{"b"}}},
	}
	h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}, 100000)
	if _, err := complete(h, contracts.RolePlanner); !errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	if n := len(h.costs.Events()); n != 0 {
		t.Fatalf("failed calls recorded %d cost events", n)
	}
}

// stubEmbedder is a Provider that also serves embeddings.
type stubEmbedder struct {
	*faketest.Provider
	calls int
}

func (s *stubEmbedder) Embed(_ context.Context, _ string, texts []string) (llm.EmbedResponse, error) {
	s.calls++
	return llm.EmbedResponse{ID: fmt.Sprintf("emb_%d", s.calls), Vectors: make([][]float32, len(texts)), InputTokens: 100}, nil
}

// TestEmbedTierMatrix: embeddings pass the same tier check as completions.
func TestEmbedTierMatrix(t *testing.T) {
	for _, tier := range allTiers {
		for _, provider := range allProviders {
			t.Run(fmt.Sprintf("%s/%s", tier, provider), func(t *testing.T) {
				emb := &stubEmbedder{Provider: faketest.New()}
				p := providers(fakes())
				p[provider] = emb
				cfg := llm.Config{
					Models:    map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic), "e": model(provider)},
					Roles:     map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
					Embedding: &llm.EmbeddingRoute{Model: "e"},
				}
				r, err := llm.New(cfg, p, llm.StaticRepos{repoID: policy(tier, contracts.RolePlanner)}, &faketest.Costs{})
				if err != nil {
					t.Fatal(err)
				}
				s, err := r.Scope(llm.Scope{KeyPrefix: "k", TokenBudget: 1000})
				if err != nil {
					t.Fatal(err)
				}
				vecs, err := s.Embed(context.Background(), repoID, []string{"a", "b"})
				assertTierOutcome(t, tierAllows[tier][provider], err)
				if tierAllows[tier][provider] != (emb.calls == 1 && len(vecs) == 2) {
					t.Fatalf("embedder called %d times, %d vectors", emb.calls, len(vecs))
				}
			})
		}
	}
}

func TestBudget(t *testing.T) {
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(endTurn(900, 300), endTurn(1, 1))
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
	}
	h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}, 1000)

	if _, err := complete(h, contracts.RolePlanner); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := h.scoped.TokensUsed(); got != 1200 {
		t.Fatalf("TokensUsed = %d, want 1200", got)
	}
	if _, err := complete(h, contracts.RolePlanner); !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Fatalf("second call: got %v, want ErrBudgetExceeded", err)
	}
	if n := len(f[llm.ProviderAnthropic].Requests()); n != 1 {
		t.Fatalf("provider received %d requests after the budget was spent", n)
	}
}

// TestRetriedScopeKeepsBudget: a scope seeded with earlier spend does not
// get a fresh budget.
func TestRetriedScopeKeepsBudget(t *testing.T) {
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
	}
	f := fakes()
	r, err := llm.New(cfg, providers(f), llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}, &faketest.Costs{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Scope(llm.Scope{KeyPrefix: "k", TokenBudget: 1000, TokensUsed: 1000})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Complete(context.Background(), contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi")})
	if !errors.Is(err, llm.ErrBudgetExceeded) {
		t.Fatalf("got %v, want ErrBudgetExceeded", err)
	}
}

func TestMaxTokensClamp(t *testing.T) {
	cases := []struct {
		name      string
		budget    int
		requested int
		want      int
	}{
		{"model maximum", 100000, 0, 4096},
		{"caller limit", 100000, 100, 100},
		// The budget also holds the input estimate, one token for "hi".
		{"remaining budget", 500, 0, 499},
		{"budget below caller limit", 500, 2000, 499},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fakes()
			f[llm.ProviderAnthropic] = faketest.New(endTurn(1, 1))
			cfg := llm.Config{
				Models: map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
				Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
			}
			h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}, tc.budget)
			req := contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi"), MaxTokens: tc.requested}
			if _, err := h.scoped.Complete(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if got := f[llm.ProviderAnthropic].Requests()[0].MaxTokens; got != tc.want {
				t.Fatalf("MaxTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestScopeRequiresKeyAndBudget(t *testing.T) {
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
	}
	r, err := llm.New(cfg, providers(fakes()), llm.StaticRepos{}, &faketest.Costs{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []llm.Scope{
		{TokenBudget: 10},
		{KeyPrefix: "k"},
		{KeyPrefix: "k", TokenBudget: 10, TokensUsed: -1},
	} {
		if _, err := r.Scope(s); !errors.Is(err, llm.ErrInvalidScope) {
			t.Errorf("Scope(%+v): got %v, want ErrInvalidScope", s, err)
		}
	}
}

func TestCostRecording(t *testing.T) {
	f := fakes()
	first := endTurn(1_000_000, 200_000)
	first.Response.ID = "msg_a"
	second := endTurn(10, 10)
	second.Response.ID = "msg_b"
	f[llm.ProviderAnthropic] = faketest.New(first, second)
	cfg := llm.Config{
		Models:    map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
		Roles:     map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
		RateLimit: llm.RateLimit{TokensPerMinute: 10_000_000},
	}
	h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}, 10_000_000)
	resp, err := complete(h, contracts.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := complete(h, contracts.RolePlanner); err != nil {
		t.Fatal(err)
	}
	// 1M input at $3 plus 0.2M output at $15.
	if resp.Usage.CostUSD != 6 {
		t.Fatalf("CostUSD = %v, want 6", resp.Usage.CostUSD)
	}
	evs := h.costs.Events()
	if len(evs) != 2 {
		t.Fatalf("%d events, want 2", len(evs))
	}
	ev := evs[0]
	if ev.IdempotencyKey != "wf/act/anthropic/msg_a" || evs[1].IdempotencyKey != "wf/act/anthropic/msg_b" {
		t.Fatalf("keys %q, %q", ev.IdempotencyKey, evs[1].IdempotencyKey)
	}
	if ev.RepoID != repoID || ev.AttemptID != "attempt-1" || ev.Role != contracts.RolePlanner ||
		ev.Provider != llm.ProviderAnthropic || ev.Model != "m" || ev.CostUSD != 6 || ev.OccurredAt.IsZero() {
		t.Fatalf("event %+v", ev)
	}
}

// TestCachePricing: cache traffic is priced at its own rate when one is
// configured, and at the input rate otherwise.
func TestCachePricing(t *testing.T) {
	cases := []struct {
		name  string
		write float64
		read  float64
		want  float64
	}{
		// 1M input, 1M cache write, 1M cache read, no output.
		{"configured rates", 3.75, 0.3, 3 + 3.75 + 0.3},
		{"unset rates fall back to input", 0, 0, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := model(llm.ProviderAnthropic)
			m.CacheWriteUSDPerMTok, m.CacheReadUSDPerMTok = tc.write, tc.read
			turn := endTurn(1_000_000, 0)
			turn.Response.CacheWriteTokens, turn.Response.CacheReadTokens = 1_000_000, 1_000_000
			f := fakes()
			f[llm.ProviderAnthropic] = faketest.New(turn)
			cfg := llm.Config{
				Models: map[string]llm.ModelConfig{"m": m},
				Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
			}
			h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}, 10_000_000)
			resp, err := complete(h, contracts.RolePlanner)
			if err != nil {
				t.Fatal(err)
			}
			if diff := resp.Usage.CostUSD - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("CostUSD = %v, want %v", resp.Usage.CostUSD, tc.want)
			}
			if resp.Usage.InputTokens != 3_000_000 || h.scoped.TokensUsed() != 3_000_000 {
				t.Fatalf("input %d, used %d: cache tokens must count", resp.Usage.InputTokens, h.scoped.TokensUsed())
			}
		})
	}
}

// TestRetriedActivityRecordsItsOwnSpend: a retry makes new calls whose
// replies differ from the first run's. Their spend is real and must be
// recorded in full, not swallowed as a duplicate of the first run's keys.
func TestRetriedActivityRecordsItsOwnSpend(t *testing.T) {
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
	}
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(endTurn(5, 5), endTurn(500, 500))
	costs := &faketest.Costs{}
	r, err := llm.New(cfg, providers(f), llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}, costs)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := r.Scope(llm.Scope{KeyPrefix: "wf/act", TokenBudget: 10_000})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Complete(context.Background(), contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi")}); err != nil {
			t.Fatal(err)
		}
	}
	total := 0
	for _, ev := range costs.Events() {
		total += ev.InputTokens + ev.OutputTokens
	}
	if total != 1010 {
		t.Fatalf("recorded %d tokens across %d events, want 1010", total, len(costs.Events()))
	}
}

// TestSameReplyRecordedOnce: recording the same provider reply twice, as a
// retried write would, counts it once.
func TestSameReplyRecordedOnce(t *testing.T) {
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
	}
	turn := endTurn(5, 5)
	turn.Response.ID = "msg_same"
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(turn, turn)
	h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}, 10_000)
	for range 2 {
		if _, err := complete(h, contracts.RolePlanner); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(h.costs.Events()); n != 1 {
		t.Fatalf("%d cost events, want 1", n)
	}
}

func TestCostRecordingFailureFailsTheCall(t *testing.T) {
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
	}
	f := fakes()
	f[llm.ProviderAnthropic] = faketest.New(endTurn(5, 5))
	boom := errors.New("database down")
	r, err := llm.New(cfg, providers(f), llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner)}, &faketest.Costs{Err: boom})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Scope(llm.Scope{KeyPrefix: "k", TokenBudget: 1000})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Complete(context.Background(), contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi")})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the recorder's error", err)
	}
}

func TestInvalidRequests(t *testing.T) {
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": model(llm.ProviderAnthropic)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
	}
	repos := llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RolePlanner, contracts.RoleBrief)}
	cases := []struct {
		name string
		req  contracts.Request
		want error
	}{
		{"no role", contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Messages: userMessage("hi")}, llm.ErrInvalidRequest},
		{"no prompt version", contracts.Request{RepoID: repoID, Role: contracts.RolePlanner, Messages: userMessage("hi")}, llm.ErrInvalidRequest},
		{"no messages", contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner}, llm.ErrInvalidRequest},
		{"negative max tokens", contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RolePlanner, Messages: userMessage("hi"), MaxTokens: -1}, llm.ErrInvalidRequest},
		{"allowed role with no route", contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RoleBrief, Messages: userMessage("hi")}, llm.ErrNoRoute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, cfg, fakes(), repos, 1000)
			if _, err := h.scoped.Complete(context.Background(), tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewRequiresAdapterForEveryProvider(t *testing.T) {
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": model(llm.ProviderGoogle)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
	}
	p := providers(fakes())
	delete(p, llm.ProviderGoogle)
	if _, err := llm.New(cfg, p, llm.StaticRepos{}, &faketest.Costs{}); !errors.Is(err, llm.ErrMissingProvider) {
		t.Fatalf("got %v, want ErrMissingProvider", err)
	}
}

// TestReplayMultiTurnThroughRouter drives a recorded conversation with two
// tool rounds through the router, the way the agent loop will.
func TestReplayMultiTurnThroughRouter(t *testing.T) {
	fake, err := faketest.Load("faketest/testdata/two_tool_rounds.json")
	if err != nil {
		t.Fatal(err)
	}
	f := fakes()
	f[llm.ProviderAnthropic] = fake
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"claude-fable-5-1": model(llm.ProviderAnthropic)},
		Roles:  map[contracts.Role]llm.RoleRoute{contracts.RoleImplementer: {Model: "claude-fable-5-1"}},
	}
	h := newHarness(t, cfg, f, llm.StaticRepos{repoID: policy(contracts.TierPublic, contracts.RoleImplementer)}, 100000)

	msgs := userMessage("Fix issue #12")
	var finished bool
	for step := 0; step < 5 && !finished; step++ {
		resp, err := h.scoped.Complete(context.Background(), contracts.Request{RepoID: repoID, PromptVersion: "test-v1", Role: contracts.RoleImplementer, Messages: msgs})
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		msgs = append(msgs, contracts.Message{Role: contracts.MessageAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
		var results []contracts.ToolResult
		for _, c := range resp.ToolCalls {
			if c.Name == contracts.ToolFinish {
				finished = true
				var report struct{ Summary string }
				if err := json.Unmarshal(c.Arguments, &report); err != nil || report.Summary == "" {
					t.Fatalf("finish arguments %s: %v", c.Arguments, err)
				}
			}
			results = append(results, contracts.ToolResult{CallID: c.ID, Output: "done"})
		}
		msgs = append(msgs, contracts.Message{Role: contracts.MessageUser, ToolResults: results})
	}
	if !finished || fake.Remaining() != 0 {
		t.Fatalf("finished=%v, %d turns unserved", finished, fake.Remaining())
	}
	if got, want := h.scoped.TokensUsed(), 1280+2440+2660; got != want {
		t.Fatalf("TokensUsed = %d, want %d", got, want)
	}
	if n := len(h.costs.Events()); n != 3 {
		t.Fatalf("%d cost events, want 3", n)
	}
}
