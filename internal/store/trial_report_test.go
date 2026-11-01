package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/store"
)

// trialSeed inserts trial rows directly with fixed timestamps, inside the
// test's transaction: the store's create functions stamp now(), and the
// report is about time.
type trialSeed struct {
	t    *testing.T
	ctx  context.Context
	tx   pgx.Tx
	repo string
}

func (s trialSeed) id(query string, args ...any) string {
	s.t.Helper()
	var id string
	if err := s.tx.QueryRow(s.ctx, query+` RETURNING id::text`, args...).Scan(&id); err != nil {
		s.t.Fatalf("seeding: %v\n%s", err, query)
	}
	return id
}

func (s trialSeed) exec(query string, args ...any) {
	s.t.Helper()
	if _, err := s.tx.Exec(s.ctx, query, args...); err != nil {
		s.t.Fatalf("seeding: %v\n%s", err, query)
	}
}

func (s trialSeed) feature(issue int, state string, created time.Time) string {
	return s.id(`INSERT INTO feature_requests (repo_id, issue_number, title, state, workflow_id, created_at)
		VALUES ($1, $2, 'trial', $3, $4, $5)`, s.repo, issue, state, fmt.Sprintf("feature/1/%d", issue), created)
}

// plan inserts an approved plan when approved is set, otherwise a
// proposed one, and its one task.
func (s trialSeed) planAndTask(feature string, approved *time.Time) string {
	state := "proposed"
	if approved != nil {
		state = "approved"
	}
	plan := s.id(`INSERT INTO plans (feature_request_id, version, state, base_sha, approved_at, body)
		VALUES ($1, 1, $2, CASE WHEN $2 = 'approved' THEN 'abc123' END, $3, '{}')`, feature, state, approved)
	return s.id(`INSERT INTO tasks (plan_id, key, spec, state) VALUES ($1, 'T1', '{}', 'running')`, plan)
}

func (s trialSeed) attempt(task string, ordinal int, state string, started time.Time, finished *time.Time) string {
	return s.id(`INSERT INTO attempts (task_id, ordinal, state, role, prompt_version, model, provider, started_at, finished_at)
		VALUES ($1, $2, $3, 'implementer', 'v1', 'claude-fable-5-1', 'anthropic', $4, $5)`, task, ordinal, state, started, finished)
}

func (s trialSeed) cost(key, feature string, attempt any, in, out int64, usd float64) {
	s.exec(`INSERT INTO cost_events
		(idempotency_key, repo_id, feature_request_id, attempt_id, role, provider, model, input_tokens, output_tokens, cost_usd)
		VALUES ($1, $2, $3, $4, 'implementer', 'anthropic', 'm', $5, $6, $7)`, key, s.repo, feature, attempt, in, out, usd)
}

func (s trialSeed) moved(feature, from, to string, at time.Time) {
	s.exec(`INSERT INTO state_transitions (entity_kind, entity_id, from_state, to_state, actor, db_user, revision, occurred_at)
		VALUES ('feature_request', $1, $2, $3, 'test', 'test', 1, $4)`, feature, from, to, at)
}

// TestTrialReport seeds a delivered trial with a retried attempt and a
// pr_writer call, a trial stopped in needs_attention, a cancelled trial,
// and one outside the window, then checks every figure, read as the
// application role.
func TestTrialReport(t *testing.T) {
	ctx, tx := testTx(t)
	s := trialSeed{t: t, ctx: ctx, tx: tx}
	s.repo = s.id(`INSERT INTO repos (full_name, tier, default_branch) VALUES ('austmsk/trial-repo', 'private', 'main')`)
	day := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	ptr := func(t time.Time) *time.Time { return &t }

	delivered := s.feature(11, "delivered", at(0, 0))
	task := s.planAndTask(delivered, ptr(at(1, 0)))
	first := s.attempt(task, 1, "errored", at(1, 0), ptr(at(1, 10)))
	second := s.attempt(task, 2, "passed", at(1, 10), ptr(at(1, 30)))
	s.cost("d-1", delivered, first, 100, 50, 0.10)
	s.cost("d-2", delivered, second, 200, 100, 0.20)
	s.cost("d-pr", delivered, nil, 10, 5, 0.05) // the pr_writer, outside any attempt
	s.moved(delivered, "executing", "delivered", at(2, 0))

	stopped := s.feature(12, "needs_attention", at(2, 0))
	stoppedTask := s.planAndTask(stopped, ptr(at(2, 30)))
	stoppedAttempt := s.attempt(stoppedTask, 1, "errored", at(3, 0), ptr(at(3, 5)))
	s.cost("n-1", stopped, stoppedAttempt, 1000, 0, 1.00)
	s.attempt(stoppedTask, 2, "running", at(3, 10), nil) // unfinished: no agent time

	cancelled := s.feature(13, "cancelled", at(3, 0))
	s.planAndTask(cancelled, nil)

	outside := s.feature(14, "delivered", day.AddDate(0, 1, 0))
	s.cost("o-1", outside, nil, 5, 5, 9.99)

	var rep store.TrialReport
	err := asAppRole(t, ctx, tx, func(sp pgx.Tx) error {
		var err error
		rep, err = store.ReadTrialReport(ctx, sp, day, day.AddDate(0, 0, 1))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []store.TrialRow{
		{FeatureRequestID: delivered, Repo: "austmsk/trial-repo", Issue: 11, State: "delivered", Attempts: 2,
			InputTokens: 310, OutputTokens: 155, CostUSD: 0.35, AgentTime: 30 * time.Minute, ApproveToDelivered: time.Hour},
		{FeatureRequestID: stopped, Repo: "austmsk/trial-repo", Issue: 12, State: "needs_attention", Attempts: 2,
			InputTokens: 1000, CostUSD: 1.00, AgentTime: 5 * time.Minute},
		{FeatureRequestID: cancelled, Repo: "austmsk/trial-repo", Issue: 13, State: "cancelled"},
	}
	checkTrialRows(t, rep.Rows, want)

	sum := rep.Summary
	if sum.Trials != 3 || sum.Delivered != 1 || sum.Cancelled != 1 {
		t.Errorf("counts %+v, want 3 trials, 1 delivered, 1 cancelled", sum)
	}
	if !closeUSD(sum.ResolveRate, 1.0/3) || !closeUSD(sum.CostPerResolvedUSD, 1.35) || sum.TimePerResolved != time.Hour {
		t.Errorf("summary %+v, want resolve rate 1/3, $1.35 per resolved task (every trial's spend), an hour per task", sum)
	}
}

// checkTrialRows compares rows to want, costs to the cent.
func checkTrialRows(t *testing.T, rows, want []store.TrialRow) {
	t.Helper()
	if len(rows) != len(want) {
		t.Fatalf("%d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		got := rows[i]
		if !closeUSD(got.CostUSD, w.CostUSD) {
			t.Errorf("row %d cost %v, want %v", i, got.CostUSD, w.CostUSD)
		}
		got.CostUSD = w.CostUSD
		if got != w {
			t.Errorf("row %d:\n got %+v\nwant %+v", i, got, w)
		}
	}
}

// TestTrialReportEmpty: a window with no trials reports zeros, never a
// division by zero, and an empty window is refused.
func TestTrialReportEmpty(t *testing.T) {
	ctx, tx := testTx(t)
	from := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	rep, err := store.ReadTrialReport(ctx, tx, from, from.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 0 || rep.Summary != (store.TrialSummary{}) {
		t.Fatalf("report %+v, want nothing", rep)
	}
	if _, err := store.ReadTrialReport(ctx, tx, from, from); err == nil {
		t.Fatal("an empty window was accepted")
	}
}

func closeUSD(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
