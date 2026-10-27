package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// errAny stands for "some error" in a case table where the error has no
// sentinel of its own.
var errAny = errors.New("any error")

// unknownID is a well-formed UUID that names no row.
const unknownID = "00000000-0000-0000-0000-000000000000"

// createCase is one call to a create function. Every case that succeeds
// must return the same row: the cases of one table all name one natural
// key, or are refused before reaching it.
type createCase struct {
	name        string
	call        func() (id string, created bool, err error)
	wantCreated bool
	wantErr     error
}

// runCreateCases runs the cases in order, in the caller's transaction and
// without savepoints, so a refusal that aborted the transaction would fail
// every case after it. It returns the ID the successful cases agreed on.
func runCreateCases(t *testing.T, cases []createCase) string {
	t.Helper()
	var rowID string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, created, err := tc.call()
			if !wantError(t, err, tc.wantErr) {
				return
			}
			if created != tc.wantCreated {
				t.Errorf("created = %v, want %v", created, tc.wantCreated)
			}
			if rowID == "" {
				rowID = id
			}
			if id != rowID {
				t.Errorf("returned row %s, want the first row %s", id, rowID)
			}
		})
	}
	return rowID
}

// wantError checks err against want, which is nil for success or errAny
// for any error, and reports whether the call succeeded as expected.
func wantError(t *testing.T, err, want error) bool {
	t.Helper()
	switch {
	case want == nil && err != nil:
		t.Fatalf("unexpected error: %v", err)
	case want == errAny && err == nil:
		t.Fatal("succeeded, want an error")
	case want != nil && want != errAny && !errors.Is(err, want):
		t.Fatalf("got %v, want %v", err, want)
	}
	return want == nil
}

// auditCount counts every audit row for the entity.
func auditCount(t *testing.T, ctx context.Context, tx pgx.Tx, id string) int {
	t.Helper()
	return countRows(t, ctx, tx, `SELECT count(*) FROM public.state_transitions WHERE entity_id = $1`, id)
}

func testRepo(name string) contracts.Repo {
	return contracts.Repo{FullName: name, Tier: contracts.TierPublic, DefaultBranch: "main"}
}

func testProvenance() contracts.Provenance {
	return contracts.Provenance{Role: contracts.RoleImplementer, PromptVersion: "implementer-v1",
		Model: "claude-fable-5-1", Provider: "anthropic"}
}

func testSpec(key string) contracts.TaskSpec {
	return contracts.TaskSpec{Key: key, Description: "Export tallies as CSV",
		Criteria: []contracts.AcceptanceCriteria{{ID: "AC1", Text: "GET /export returns text/csv"}}}
}

// workChain is one row of each work table, created through the store
// functions.
type workChain struct {
	repo    contracts.Repo
	feature contracts.FeatureRequest
	plan    contracts.Plan
	task    contracts.Task
	attempt contracts.Attempt
}

func createChain(t *testing.T, ctx context.Context, tx pgx.Tx, repoName string, issue int) workChain {
	t.Helper()
	var c workChain
	var err error
	if c.repo, err = store.EnsureRepo(ctx, tx, testRepo(repoName)); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if c.feature, _, err = store.CreateFeatureRequest(ctx, tx, store.FeatureRequestParams{
		RepoID: c.repo.ID, IssueNumber: issue, Title: "Add tally export", WorkflowID: "feature-" + repoName,
	}); err != nil {
		t.Fatalf("CreateFeatureRequest: %v", err)
	}
	if c.plan, _, err = store.CreatePlan(ctx, tx, store.PlanParams{
		FeatureRequestID: c.feature.ID, Version: 1, Body: []byte(`{"task":{"key":"T1"}}`),
	}); err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if c.task, _, err = store.CreateTask(ctx, tx, c.plan.ID, testSpec("T1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if c.attempt, _, err = store.CreateAttempt(ctx, tx, store.AttemptParams{
		TaskID: c.task.ID, Ordinal: 1, Provenance: testProvenance(),
	}); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	return c
}

// TestEnsureRepo: the first call inserts, a repeat returns the same row,
// and a changed tier or branch in configuration is written over the row
// (FR-SEC12). Each case reads back the stored row.
func TestEnsureRepo(t *testing.T) {
	ctx, tx := testTx(t)
	const name = "austmsk/ensure-repo"
	var firstID string
	cases := []struct {
		name    string
		in      contracts.Repo
		wantErr error
	}{
		{"inserts", testRepo(name), nil},
		{"same again", testRepo(name), nil},
		{"tier changed in configuration", contracts.Repo{FullName: name, Tier: contracts.TierSensitive, DefaultBranch: "main"}, nil},
		{"branch changed in configuration", contracts.Repo{FullName: name, Tier: contracts.TierSensitive, DefaultBranch: "trunk"}, nil},
		{"ID in the input is ignored", contracts.Repo{ID: unknownID, FullName: name, Tier: contracts.TierSensitive, DefaultBranch: "trunk"}, nil},
		{"empty name", contracts.Repo{Tier: contracts.TierPublic, DefaultBranch: "main"}, errAny},
		{"unknown tier", contracts.Repo{FullName: name, Tier: "secret", DefaultBranch: "main"}, errAny},
		{"no tier", contracts.Repo{FullName: name, DefaultBranch: "main"}, errAny},
		{"empty branch", contracts.Repo{FullName: name, Tier: contracts.TierPublic}, errAny},
		{"NUL in name", contracts.Repo{FullName: "a\x00b", Tier: contracts.TierPublic, DefaultBranch: "main"}, errAny},
	}
	dir := store.NewRepoDirectory(tx)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.EnsureRepo(ctx, tx, tc.in)
			if !wantError(t, err, tc.wantErr) {
				return
			}
			if firstID == "" {
				firstID = got.ID
			}
			want := contracts.Repo{ID: firstID, FullName: name, Tier: tc.in.Tier, DefaultBranch: tc.in.DefaultBranch}
			if got != want {
				t.Errorf("returned %+v, want %+v", got, want)
			}
			if stored, found, err := dir.Repo(ctx, firstID); err != nil || !found || stored != want {
				t.Errorf("stored %+v (found %v, err %v), want %+v", stored, found, err, want)
			}
		})
	}
	if n := countRows(t, ctx, tx, `SELECT count(*) FROM repos WHERE full_name = $1`, name); n != 1 {
		t.Errorf("%d rows for %s, want 1", n, name)
	}
}

func TestRepoByFullName(t *testing.T) {
	ctx, tx := testTx(t)
	want, err := store.EnsureRepo(ctx, tx, testRepo("austmsk/by-name"))
	if err != nil {
		t.Fatal(err)
	}
	dir := store.NewRepoDirectory(tx)
	cases := []struct {
		name      string
		fullName  string
		wantFound bool
	}{
		{"configured repository", "austmsk/by-name", true},
		{"unknown name", "austmsk/nothing", false},
		{"different case", "Austmsk/By-Name", false},
		{"empty", "", false},
		{"NUL", "austmsk/\x00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := dir.RepoByFullName(ctx, tc.fullName)
			if err != nil || found != tc.wantFound {
				t.Fatalf("found %v, err %v: want found %v", found, err, tc.wantFound)
			}
			if found && got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestCreateFeatureRequest(t *testing.T) {
	ctx, tx := testTx(t)
	repo, err := store.EnsureRepo(ctx, tx, testRepo("austmsk/feature"))
	if err != nil {
		t.Fatal(err)
	}
	params := func(apply func(*store.FeatureRequestParams)) func() (string, bool, error) {
		return func() (string, bool, error) {
			p := store.FeatureRequestParams{RepoID: repo.ID, IssueNumber: 12, Title: "Add tally export", WorkflowID: "feature-12"}
			apply(&p)
			f, created, err := store.CreateFeatureRequest(ctx, tx, p)
			return f.ID, created, err
		}
	}
	same := func(*store.FeatureRequestParams) {}
	id := runCreateCases(t, []createCase{
		{"creates", params(same), true, nil},
		{"same again", params(same), false, nil},
		{"new title does not overwrite", params(func(p *store.FeatureRequestParams) { p.Title = "Renamed" }), false, nil},
		{"another workflow", params(func(p *store.FeatureRequestParams) { p.WorkflowID = "feature-99" }), false, store.ErrWorkflowMismatch},
		{"malformed repository ID", params(func(p *store.FeatureRequestParams) { p.RepoID = "austmsk/feature" }), false, store.ErrInvalidID},
		{"unknown repository", params(func(p *store.FeatureRequestParams) { p.RepoID = unknownID }), false, store.ErrNotFound},
		{"issue zero", params(func(p *store.FeatureRequestParams) { p.IssueNumber = 0 }), false, errAny},
		{"no workflow", params(func(p *store.FeatureRequestParams) { p.WorkflowID = "" }), false, errAny},
		{"NUL in title", params(func(p *store.FeatureRequestParams) { p.Title = "a\x00b" }), false, errAny},
	})

	f, _, err := store.CreateFeatureRequest(ctx, tx, store.FeatureRequestParams{
		RepoID: repo.ID, IssueNumber: 12, Title: "ignored", WorkflowID: "feature-12"})
	if err != nil {
		t.Fatal(err)
	}
	want := contracts.FeatureRequest{ID: id, RepoID: repo.ID, IssueNumber: 12, Title: "Add tally export",
		Status: contracts.FeaturePlanning, WorkflowID: "feature-12", CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt}
	if f != want || f.CreatedAt.IsZero() {
		t.Errorf("stored %+v, want %+v", f, want)
	}
	if n := countRows(t, ctx, tx, `SELECT count(*) FROM feature_requests WHERE repo_id = $1`, repo.ID); n != 1 {
		t.Errorf("%d feature requests, want 1", n)
	}
	if n := auditCount(t, ctx, tx, id); n != 0 {
		t.Errorf("%d audit rows after creation, want 0: the trail starts at the first transition", n)
	}
}

func TestCreatePlan(t *testing.T) {
	ctx, tx := testTx(t)
	c := createChain(t, ctx, tx, "austmsk/plan", 1)
	const body = `{"task": {"key": "T1"}, "issue_text": "Add export"}`
	params := func(apply func(*store.PlanParams)) func() (string, bool, error) {
		return func() (string, bool, error) {
			p := store.PlanParams{FeatureRequestID: c.feature.ID, Version: 2, Body: []byte(body)}
			apply(&p)
			plan, created, err := store.CreatePlan(ctx, tx, p)
			if err == nil && (plan.Status != contracts.PlanProposed || plan.Version != 2 || len(plan.Tasks) != 0) {
				t.Errorf("returned %+v, want version 2, proposed, no tasks", plan)
			}
			return plan.ID, created, err
		}
	}
	same := func(*store.PlanParams) {}
	id := runCreateCases(t, []createCase{
		{"creates", params(same), true, nil},
		{"same again", params(same), false, nil},
		{"new body does not overwrite", params(func(p *store.PlanParams) { p.Body = []byte(`{"other":true}`) }), false, nil},
		{"malformed feature request ID", params(func(p *store.PlanParams) { p.FeatureRequestID = "issue-12" }), false, store.ErrInvalidID},
		{"unknown feature request", params(func(p *store.PlanParams) { p.FeatureRequestID = unknownID }), false, store.ErrNotFound},
		{"version zero", params(func(p *store.PlanParams) { p.Version = 0 }), false, errAny},
		{"body not JSON", params(func(p *store.PlanParams) { p.Body = []byte(`{`) }), false, errAny},
		{"no body", params(func(p *store.PlanParams) { p.Body = nil }), false, errAny},
		{"NUL escape jsonb refuses", params(func(p *store.PlanParams) { p.Body = []byte(`{"a":"\u0000"}`) }), false, errAny},
	})

	var same1 bool
	err := tx.QueryRow(ctx, `SELECT body = $2::jsonb FROM plans WHERE id = $1`, id, body).Scan(&same1)
	if err != nil || !same1 {
		t.Errorf("stored body equals the first call's: %v (err %v)", same1, err)
	}
	if n := countRows(t, ctx, tx, `SELECT count(*) FROM plans WHERE feature_request_id = $1 AND version = 2`, c.feature.ID); n != 1 {
		t.Errorf("%d rows for version 2, want 1", n)
	}
	if n := auditCount(t, ctx, tx, id); n != 0 {
		t.Errorf("%d audit rows after creation, want 0", n)
	}
}

func TestCreateTask(t *testing.T) {
	ctx, tx := testTx(t)
	c := createChain(t, ctx, tx, "austmsk/task", 1)
	spec := testSpec("T2-export")
	call := func(planID string, s contracts.TaskSpec) func() (string, bool, error) {
		return func() (string, bool, error) {
			task, created, err := store.CreateTask(ctx, tx, planID, s)
			if err == nil && (task.Status != contracts.TaskRunning || !reflect.DeepEqual(task.Spec, spec)) {
				t.Errorf("returned %+v, want running with the first spec", task)
			}
			return task.ID, created, err
		}
	}
	changed := spec
	changed.Description = "Something else"
	id := runCreateCases(t, []createCase{
		{"creates", call(c.plan.ID, spec), true, nil},
		{"same again", call(c.plan.ID, spec), false, nil},
		{"new spec does not overwrite", call(c.plan.ID, changed), false, nil},
		{"malformed plan ID", call("plan-1", spec), false, store.ErrInvalidID},
		{"unknown plan", call(unknownID, spec), false, store.ErrNotFound},
		{"empty key", call(c.plan.ID, contracts.TaskSpec{Description: "x"}), false, errAny},
		{"NUL in description", call(c.plan.ID, contracts.TaskSpec{Key: "T3", Description: "a\x00b"}), false, errAny},
	})

	if n := countRows(t, ctx, tx, `SELECT count(*) FROM tasks WHERE plan_id = $1 AND key = 'T2-export'`, c.plan.ID); n != 1 {
		t.Errorf("%d rows for the key, want 1", n)
	}
	if n := auditCount(t, ctx, tx, id); n != 0 {
		t.Errorf("%d audit rows after creation, want 0", n)
	}
}

func TestCreateAttempt(t *testing.T) {
	ctx, tx := testTx(t)
	c := createChain(t, ctx, tx, "austmsk/attempt", 1)
	call := func(apply func(*store.AttemptParams)) func() (string, bool, error) {
		return func() (string, bool, error) {
			p := store.AttemptParams{TaskID: c.task.ID, Ordinal: 2, Provenance: testProvenance()}
			apply(&p)
			a, created, err := store.CreateAttempt(ctx, tx, p)
			if err == nil && (a.State != contracts.AttemptRunning || a.Ordinal != 2 ||
				a.Provenance != testProvenance() || a.StartedAt.IsZero() || a.Report != nil) {
				t.Errorf("returned %+v, want running ordinal 2 with the first provenance", a)
			}
			return a.ID, created, err
		}
	}
	same := func(*store.AttemptParams) {}
	id := runCreateCases(t, []createCase{
		{"creates", call(same), true, nil},
		{"same again", call(same), false, nil},
		{"new provenance does not overwrite", call(func(p *store.AttemptParams) { p.Provenance.Model = "other" }), false, nil},
		{"malformed task ID", call(func(p *store.AttemptParams) { p.TaskID = "T1" }), false, store.ErrInvalidID},
		{"unknown task", call(func(p *store.AttemptParams) { p.TaskID = unknownID }), false, store.ErrNotFound},
		{"ordinal zero", call(func(p *store.AttemptParams) { p.Ordinal = 0 }), false, errAny},
		{"no role", call(func(p *store.AttemptParams) { p.Provenance.Role = "" }), false, errAny},
		{"no prompt version", call(func(p *store.AttemptParams) { p.Provenance.PromptVersion = "" }), false, errAny},
		{"no model", call(func(p *store.AttemptParams) { p.Provenance.Model = "" }), false, errAny},
		{"no provider", call(func(p *store.AttemptParams) { p.Provenance.Provider = "" }), false, errAny},
	})

	if n := countRows(t, ctx, tx, `SELECT count(*) FROM attempts WHERE task_id = $1 AND ordinal = 2`, c.task.ID); n != 1 {
		t.Errorf("%d rows for ordinal 2, want 1", n)
	}
	if n := auditCount(t, ctx, tx, id); n != 0 {
		t.Errorf("%d audit rows after creation, want 0", n)
	}
}
