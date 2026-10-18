package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
)

// These tests run the real router over the real ledger, with every call on
// its own pooled connection as the application role, so the caps are held
// by the reservation transaction and nothing in process memory. Every call
// asks for 100 output tokens of "hi", which estimates at one input token,
// so each claims 101 tokens, and the provider bills exactly that.
const callTokens = 101

// callUSD is what one call claims and is billed: 1 input token at $3 and
// 100 output tokens at $15 per million.
const callUSD = (1*3 + 100*15) / 1e6

// appPool connects as the application role on every connection.
func appPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 24
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE conclave_app`)
		return err
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// chain is a committed repository, feature request, plan, and task with
// some attempts.
type chain struct {
	repo, feature string
	attempts      []string
}

// committedChain inserts a chain as the owner and commits it. Its rows and
// everything charged to it are removed when the test ends.
func committedChain(t *testing.T, ctx context.Context, attempts int) chain {
	t.Helper()
	var c chain
	var plan, task string
	steps := []struct {
		dst  *string
		sql  string
		args func() []any
	}{
		{&c.repo, `INSERT INTO repos (full_name, tier, default_branch) VALUES ($1, 'public', 'main') RETURNING id::text`,
			func() []any { return []any{fmt.Sprintf("budget/%s/%d", t.Name(), time.Now().UnixNano())} }},
		{&c.feature, `INSERT INTO feature_requests (repo_id, issue_number, title, state, workflow_id)
			VALUES ($1, 1, 'budget', 'executing', 'feature-budget') RETURNING id::text`, func() []any { return []any{c.repo} }},
		{&plan, `INSERT INTO plans (feature_request_id, version, state, body) VALUES ($1, 1, 'proposed', '{}') RETURNING id::text`,
			func() []any { return []any{c.feature} }},
		{&task, `INSERT INTO tasks (plan_id, key, spec, state) VALUES ($1, 'T1', '{}', 'running') RETURNING id::text`,
			func() []any { return []any{plan} }},
	}
	for _, st := range steps {
		if err := pool.QueryRow(ctx, st.sql, st.args()...).Scan(st.dst); err != nil {
			t.Fatalf("seeding chain: %v", err)
		}
	}
	for i := range attempts {
		var id string
		err := pool.QueryRow(ctx, `INSERT INTO attempts (task_id, ordinal, state, role, prompt_version, model, provider)
			VALUES ($1, $2, 'running', 'implementer', 'v1', 'm', 'anthropic') RETURNING id::text`, task, i+1).Scan(&id)
		if err != nil {
			t.Fatalf("seeding attempt: %v", err)
		}
		c.attempts = append(c.attempts, id)
	}
	t.Cleanup(func() { removeChain(t, c, plan, task) })
	return c
}

func removeChain(t *testing.T, c chain, plan, task string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cleanup := []struct{ sql, id string }{
		{`DELETE FROM cost_events WHERE repo_id = $1`, c.repo},
		{`DELETE FROM budget_reservations WHERE feature_request_id = $1`, c.feature},
		{`DELETE FROM attempts WHERE task_id = $1`, task},
		{`DELETE FROM tasks WHERE id = $1`, task},
		{`DELETE FROM plans WHERE id = $1`, plan},
		{`DELETE FROM feature_requests WHERE id = $1`, c.feature},
		{`DELETE FROM repos WHERE id = $1`, c.repo},
	}
	for _, step := range cleanup {
		if _, err := pool.Exec(ctx, step.sql, step.id); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}
}

// billingProvider replies at once, billing one input token and every
// output token it was allowed. When gate is set, each call first reports
// on started and waits on gate.
type billingProvider struct {
	ids     atomic.Int64
	started chan struct{}
	gate    chan struct{}
}

func (p *billingProvider) Complete(ctx context.Context, req llm.ProviderRequest) (llm.ProviderResponse, error) {
	if p.gate != nil {
		p.started <- struct{}{}
		select {
		case <-p.gate:
		case <-ctx.Done():
			return llm.ProviderResponse{}, ctx.Err()
		}
	}
	return llm.ProviderResponse{
		ID: fmt.Sprintf("msg_%d", p.ids.Add(1)), Text: "ok", StopReason: contracts.StopEndTurn,
		InputTokens: 1, OutputTokens: req.MaxTokens,
	}, nil
}

// budgetRouter is a router over a ledger on the application pool, for the
// given chains' repositories.
func budgetRouter(t *testing.T, ctx context.Context, p llm.Provider, budgets llm.Budgets, chains ...chain) *llm.Router {
	t.Helper()
	cfg := llm.Config{
		Models: map[string]llm.ModelConfig{"m": {
			Provider: llm.ProviderAnthropic, InputUSDPerMTok: 3, OutputUSDPerMTok: 15, MaxOutputTokens: 4096,
		}},
		Roles:     map[contracts.Role]llm.RoleRoute{contracts.RolePlanner: {Model: "m"}},
		RateLimit: llm.RateLimit{RequestsPerMinute: 1_000_000, TokensPerMinute: 1_000_000_000},
		Budgets:   budgets,
	}
	repos := llm.StaticRepos{}
	for _, c := range chains {
		repos[c.repo] = llm.RepoPolicy{Tier: contracts.TierPublic, Roles: []contracts.Role{contracts.RolePlanner}}
	}
	r, err := llm.New(cfg, map[string]llm.Provider{llm.ProviderAnthropic: p}, repos, store.NewCostLedger(appPool(t, ctx)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func attemptScope(t *testing.T, ctx context.Context, r *llm.Router, c chain, attempt, prefix string, budget int) *llm.Scoped {
	t.Helper()
	s, err := r.Scope(ctx, llm.Scope{RepoID: c.repo, FeatureRequestID: c.feature, AttemptID: attempt, KeyPrefix: prefix, TokenBudget: budget})
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	return s
}

func hiRequest(repo string) contracts.Request {
	return contracts.Request{
		RepoID: repo, PromptVersion: "test-v1", Role: contracts.RolePlanner, MaxTokens: 100,
		Messages: []contracts.Message{{Role: contracts.MessageUser, Text: "hi"}},
	}
}

// burst sends calls requests on each scope at once and counts how many
// succeeded. Every failure must be a refusal by the cap of kind want.
func burst(t *testing.T, ctx context.Context, scopes []*llm.Scoped, repos []string, calls int, want llm.BudgetKind) int {
	t.Helper()
	var wg sync.WaitGroup
	var ok atomic.Int64
	errs := make(chan error, len(scopes)*calls)
	for i, s := range scopes {
		for range calls {
			wg.Go(func() {
				_, err := s.Complete(ctx, hiRequest(repos[i]))
				var be llm.BudgetError
				switch {
				case err == nil:
					ok.Add(1)
				case !errors.As(err, &be) || be.Kind != want:
					errs <- err
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("want success or a %s refusal, got %v", want, err)
	}
	return int(ok.Load())
}

// settled sums the tokens and dollars recorded for a feature request, and
// counts its live reservations.
func settled(t *testing.T, ctx context.Context, feature string) (tokens int, usd float64, reservations int) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT
		(SELECT coalesce(sum(input_tokens + output_tokens), 0)::bigint FROM cost_events WHERE feature_request_id = $1),
		(SELECT coalesce(sum(cost_usd), 0)::float8 FROM cost_events WHERE feature_request_id = $1),
		(SELECT count(*) FROM budget_reservations WHERE feature_request_id = $1)`, feature).Scan(&tokens, &usd, &reservations)
	if err != nil {
		t.Fatal(err)
	}
	return tokens, usd, reservations
}

// TestInFlightCallCountsAcrossScopes attempts the overspend the
// reservations close, across database connections: an abandoned
// activity's call is still at the provider, unsettled, when its retry
// opens a second scope for the same attempt and calls. The retry is
// refused before the provider; the abandoned call then settles normally.
func TestInFlightCallCountsAcrossScopes(t *testing.T) {
	ctx := requireDB(t)
	c := committedChain(t, ctx, 1)
	p := &billingProvider{started: make(chan struct{}, 1), gate: make(chan struct{}, 1)}
	r := budgetRouter(t, ctx, p, llm.Budgets{FeatureRequestTokens: 1 << 40, MonthlyUSD: 1e9}, c)
	abandoned := attemptScope(t, ctx, r, c, c.attempts[0], "wf/act-1", callTokens)
	retry := attemptScope(t, ctx, r, c, c.attempts[0], "wf/act-2", callTokens)

	first := make(chan error, 1)
	go func() {
		_, err := abandoned.Complete(ctx, hiRequest(c.repo))
		first <- err
	}()
	<-p.started
	_, err := retry.Complete(ctx, hiRequest(c.repo))
	var be llm.BudgetError
	if !errors.As(err, &be) || be.Kind != llm.BudgetAttempt {
		t.Fatalf("retry while the first call is in flight: got %v, want an attempt budget refusal", err)
	}
	p.gate <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("in-flight call: %v", err)
	}
	if tokens, _, held := settled(t, ctx, c.feature); tokens != callTokens || held != 0 {
		t.Fatalf("%d tokens recorded and %d reservations, want %d and 0", tokens, held, callTokens)
	}
	if n := p.ids.Load(); n != 1 {
		t.Fatalf("provider billed %d calls, want 1", n)
	}
}

// TestConcurrentScopesStayInAttemptBudget: two scopes for one attempt send
// twenty calls at once against a budget of eight. Exactly eight run.
func TestConcurrentScopesStayInAttemptBudget(t *testing.T) {
	ctx := requireDB(t)
	c := committedChain(t, ctx, 1)
	r := budgetRouter(t, ctx, &billingProvider{}, llm.Budgets{FeatureRequestTokens: 1 << 40, MonthlyUSD: 1e9}, c)
	const fits = 8
	scopes := []*llm.Scoped{
		attemptScope(t, ctx, r, c, c.attempts[0], "wf/act-1", fits*callTokens),
		attemptScope(t, ctx, r, c, c.attempts[0], "wf/act-2", fits*callTokens),
	}
	ok := burst(t, ctx, scopes, []string{c.repo, c.repo}, 10, llm.BudgetAttempt)
	tokens, _, held := settled(t, ctx, c.feature)
	if ok != fits || tokens != fits*callTokens || held != 0 {
		t.Fatalf("%d calls ran, %d tokens recorded, %d reservations left; want %d, %d, 0", ok, tokens, held, fits, fits*callTokens)
	}
}

// TestConcurrentAttemptsStayInFeatureRequestCap: four attempts of one
// feature request, each with a budget of its own to spare, send twenty
// calls at once against a feature request cap of seven.
func TestConcurrentAttemptsStayInFeatureRequestCap(t *testing.T) {
	ctx := requireDB(t)
	c := committedChain(t, ctx, 4)
	const fits = 7
	r := budgetRouter(t, ctx, &billingProvider{}, llm.Budgets{FeatureRequestTokens: fits * callTokens, MonthlyUSD: 1e9}, c)
	var scopes []*llm.Scoped
	var repos []string
	for i, a := range c.attempts {
		scopes = append(scopes, attemptScope(t, ctx, r, c, a, fmt.Sprintf("wf/act-%d", i), 1<<30))
		repos = append(repos, c.repo)
	}
	ok := burst(t, ctx, scopes, repos, 5, llm.BudgetFeatureRequest)
	tokens, _, held := settled(t, ctx, c.feature)
	if ok != fits || tokens != fits*callTokens || held != 0 {
		t.Fatalf("%d calls ran, %d tokens recorded, %d reservations left; want %d, %d, 0", ok, tokens, held, fits, fits*callTokens)
	}
}

// TestConcurrentCallsStayInMonthlyCap: attempts in two repositories send
// twenty calls at once against a monthly cap with room for seven and a
// half more calls than this month has already spent. The cap is across
// repositories, so seven run between them.
func TestConcurrentCallsStayInMonthlyCap(t *testing.T) {
	ctx := requireDB(t)
	a, b := committedChain(t, ctx, 2), committedChain(t, ctx, 2)
	var before float64
	err := pool.QueryRow(ctx, `SELECT coalesce(sum(cost_usd), 0)::float8 FROM cost_events
		WHERE occurred_at >= date_trunc('month', statement_timestamp() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&before)
	if err != nil {
		t.Fatal(err)
	}
	const fits = 7
	r := budgetRouter(t, ctx, &billingProvider{}, llm.Budgets{FeatureRequestTokens: 1 << 40, MonthlyUSD: before + 7.5*callUSD}, a, b)
	var scopes []*llm.Scoped
	var repos []string
	for _, c := range []chain{a, b} {
		for i, attempt := range c.attempts {
			scopes = append(scopes, attemptScope(t, ctx, r, c, attempt, fmt.Sprintf("wf/act-%d", i), 1<<30))
			repos = append(repos, c.repo)
		}
	}
	ok := burst(t, ctx, scopes, repos, 5, llm.BudgetMonth)
	_, usdA, heldA := settled(t, ctx, a.feature)
	_, usdB, heldB := settled(t, ctx, b.feature)
	if ok != fits || heldA+heldB != 0 || usdA+usdB > 7.5*callUSD {
		t.Fatalf("%d calls ran, $%v recorded, %d reservations left; want %d, at most $%v, 0",
			ok, usdA+usdB, heldA+heldB, fits, 7.5*callUSD)
	}
}
