package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
)

// Sentinel errors returned by the work-table functions. Each means a
// second call named an existing row by its natural key but disagrees with
// what that row already holds on a field that must not change. None is
// retryable: the caller has confused two runs and must stop.
var (
	// ErrWorkflowMismatch means the issue already has a feature request
	// owned by a different workflow. One issue belongs to one workflow
	// (RL-14).
	ErrWorkflowMismatch = errors.New("feature request belongs to another workflow")

	// ErrSandboxMismatch means the attempt already records a different
	// sandbox. Overwriting it would lose the only name the cleanup has for
	// the first one (RL-3).
	ErrSandboxMismatch = errors.New("attempt already records a different sandbox")

	// ErrArtifactMismatch means the attempt already records a different
	// artifact hash. The hash is write-once (FR-SEC27); migration 00007's
	// trigger refuses the change in the database as well.
	ErrArtifactMismatch = errors.New("attempt already records a different artifact hash")
)

// knownTiers are the values the repos.tier CHECK accepts, checked before
// any SQL runs.
var knownTiers = map[contracts.SensitivityTier]bool{
	contracts.TierPublic: true, contracts.TierPrivate: true,
	contracts.TierSensitive: true, contracts.TierLocalOnly: true,
}

// checkText refuses a string Postgres cannot store in a text column:
// invalid UTF-8 or a NUL byte. Refused by the database instead, it would
// abort the caller's transaction.
func checkText(field, s string) error {
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return fmt.Errorf("%s: text that Postgres cannot store: invalid UTF-8 or NUL", field)
	}
	return nil
}

// checkJSONB refuses a document jsonb would reject. jsonb cannot hold the
// escape \u0000; the check is conservative and also refuses an escaped
// backslash followed by "u0000", which no document the platform writes
// contains.
func checkJSONB(field string, doc []byte) error {
	if !json.Valid(doc) {
		return fmt.Errorf("%s is not valid JSON", field)
	}
	if bytes.Contains(doc, []byte(`\u0000`)) {
		return fmt.Errorf("%s contains a NUL escape, which jsonb cannot store", field)
	}
	return nil
}

// utc converts a nullable timestamp to the contracts' convention: UTC, and
// the zero time for NULL.
func utc(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

const repoColumns = `id::text, full_name, tier, default_branch`

func scanRepo(row pgx.Row) (contracts.Repo, error) {
	var r contracts.Repo
	err := row.Scan(&r.ID, &r.FullName, &r.Tier, &r.DefaultBranch)
	return r, err
}

// EnsureRepo makes the repository's row match r, inserting it if absent,
// and returns the row with its ID. r.ID is ignored. Configuration is the
// authority for tier and default branch (FR-SEC12), so an existing row
// that disagrees is updated to match rather than reported. The worker
// calls this at startup for every configured repository.
func EnsureRepo(ctx context.Context, db DBTX, r contracts.Repo) (contracts.Repo, error) {
	switch {
	case r.FullName == "":
		return contracts.Repo{}, errors.New("ensuring repository: empty full name")
	case !knownTiers[r.Tier]:
		return contracts.Repo{}, fmt.Errorf("ensuring repository %s: unknown tier %q", r.FullName, r.Tier)
	case r.DefaultBranch == "":
		return contracts.Repo{}, fmt.Errorf("ensuring repository %s: empty default branch", r.FullName)
	}
	for _, f := range []struct{ name, v string }{{"full name", r.FullName}, {"default branch", r.DefaultBranch}} {
		if err := checkText(f.name, f.v); err != nil {
			return contracts.Repo{}, fmt.Errorf("ensuring repository: %w", err)
		}
	}
	// DO UPDATE rather than DO NOTHING so the row always comes back from
	// RETURNING; updated_at moves only when something changed.
	const upsert = `INSERT INTO repos (full_name, tier, default_branch) VALUES ($1, $2, $3)
		ON CONFLICT (full_name) DO UPDATE SET
			tier = EXCLUDED.tier,
			default_branch = EXCLUDED.default_branch,
			updated_at = CASE
				WHEN (repos.tier, repos.default_branch) IS DISTINCT FROM (EXCLUDED.tier, EXCLUDED.default_branch)
				THEN now() ELSE repos.updated_at END
		RETURNING ` + repoColumns
	got, err := scanRepo(db.QueryRow(ctx, upsert, r.FullName, string(r.Tier), r.DefaultBranch))
	if err != nil {
		return contracts.Repo{}, fmt.Errorf("ensuring repository %s: %w", r.FullName, err)
	}
	return got, nil
}

// RepoByFullName returns the repository with the given full name, such as
// "owner/repo". A name no repository has reports found false with a nil
// error, as Repo does for an unknown ID.
func (d *RepoDirectory) RepoByFullName(ctx context.Context, fullName string) (contracts.Repo, bool, error) {
	if fullName == "" || checkText("full name", fullName) != nil {
		return contracts.Repo{}, false, nil
	}
	r, err := scanRepo(d.db.QueryRow(ctx, `SELECT `+repoColumns+` FROM repos WHERE full_name = $1`, fullName))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Repo{}, false, nil
	}
	if err != nil {
		return contracts.Repo{}, false, fmt.Errorf("reading repository %s: %w", fullName, err)
	}
	return r, true, nil
}

// FeatureRequestParams describes a feature request to create.
type FeatureRequestParams struct {
	RepoID      string
	IssueNumber int
	Title       string

	// WorkflowID is the Temporal workflow that owns the issue (RL-14).
	WorkflowID string
}

const featureColumns = `id::text, repo_id::text, issue_number, title, state, coalesce(reason, ''),
	revision, workflow_id, created_at, updated_at`

func scanFeatureRequest(row pgx.Row) (contracts.FeatureRequest, error) {
	var f contracts.FeatureRequest
	err := row.Scan(&f.ID, &f.RepoID, &f.IssueNumber, &f.Title, &f.Status, &f.Reason,
		&f.Revision, &f.WorkflowID, &f.CreatedAt, &f.UpdatedAt)
	f.CreatedAt, f.UpdatedAt = f.CreatedAt.UTC(), f.UpdatedAt.UTC()
	return f, err
}

// CreateFeatureRequest records an issue in state planning and reports
// whether this call created it. The natural key is (repository, issue
// number): a second call returns the existing row unchanged, including its
// current state and title, unless that row belongs to another workflow,
// which is ErrWorkflowMismatch. An unknown repository is ErrNotFound.
//
// Creating a row writes no audit entry: the state_transitions trigger
// fires on changes to a state column, and the trail starts at the first
// transition.
func CreateFeatureRequest(ctx context.Context, db DBTX, p FeatureRequestParams) (contracts.FeatureRequest, bool, error) {
	if err := checkFeatureRequestParams(p); err != nil {
		return contracts.FeatureRequest{}, false, fmt.Errorf("creating feature request: %w", err)
	}
	// INSERT ... SELECT WHERE EXISTS turns a missing repository into zero
	// rows rather than a foreign-key violation, which would abort the
	// caller's transaction.
	const insert = `INSERT INTO feature_requests (repo_id, issue_number, title, state, workflow_id)
		SELECT $1::uuid, $2::integer, $3::text, 'planning', $4::text WHERE EXISTS (SELECT 1 FROM repos WHERE id = $1)
		ON CONFLICT (repo_id, issue_number) DO NOTHING
		RETURNING ` + featureColumns
	f, err := scanFeatureRequest(db.QueryRow(ctx, insert, p.RepoID, p.IssueNumber, p.Title, p.WorkflowID))
	if err == nil {
		return f, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return contracts.FeatureRequest{}, false, fmt.Errorf("creating feature request for issue %d: %w", p.IssueNumber, err)
	}
	const existing = `SELECT ` + featureColumns + ` FROM feature_requests WHERE repo_id = $1 AND issue_number = $2`
	f, err = scanFeatureRequest(db.QueryRow(ctx, existing, p.RepoID, p.IssueNumber))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.FeatureRequest{}, false, fmt.Errorf("creating feature request for issue %d: repository %s: %w",
			p.IssueNumber, p.RepoID, ErrNotFound)
	}
	if err != nil {
		return contracts.FeatureRequest{}, false, fmt.Errorf("reading feature request for issue %d: %w", p.IssueNumber, err)
	}
	if f.WorkflowID != p.WorkflowID {
		return contracts.FeatureRequest{}, false, fmt.Errorf("issue %d is owned by workflow %q, not %q: %w",
			p.IssueNumber, f.WorkflowID, p.WorkflowID, ErrWorkflowMismatch)
	}
	return f, false, nil
}

func checkFeatureRequestParams(p FeatureRequestParams) error {
	if err := checkUUID("repository ID", p.RepoID); err != nil {
		return err
	}
	switch {
	case p.IssueNumber <= 0:
		return fmt.Errorf("issue number %d is not positive", p.IssueNumber)
	case p.WorkflowID == "":
		return errors.New("empty workflow ID")
	}
	if err := checkText("title", p.Title); err != nil {
		return err
	}
	return checkText("workflow ID", p.WorkflowID)
}

// PlanParams describes a plan version to create.
type PlanParams struct {
	FeatureRequestID string
	Version          int

	// Body is the plan document as JSON, stored as jsonb. The store does
	// not interpret it.
	Body []byte
}

const planColumns = `id::text, feature_request_id::text, version, state, coalesce(reason, ''), revision,
	coalesce(base_sha, ''), created_at, approved_at`

func scanPlan(row pgx.Row) (contracts.Plan, error) {
	var p contracts.Plan
	var approved *time.Time
	err := row.Scan(&p.ID, &p.FeatureRequestID, &p.Version, &p.Status, &p.Reason, &p.Revision,
		&p.BaseSHA, &p.CreatedAt, &approved)
	p.CreatedAt, p.ApprovedAt = p.CreatedAt.UTC(), utc(approved)
	return p, err
}

// CreatePlan records a plan version in state proposed and reports whether
// this call created it. The natural key is (feature request, version): a
// second call returns the existing row unchanged, whatever body it was
// given. An unknown feature request is ErrNotFound.
//
// The body is opaque to the store, so the returned Plan's Tasks is always
// empty: the plan document is richer than a list of task specs, and its
// readers decode plans.body themselves. Tasks are rows of their own,
// created by CreateTask.
func CreatePlan(ctx context.Context, db DBTX, p PlanParams) (contracts.Plan, bool, error) {
	if err := checkUUID("feature request ID", p.FeatureRequestID); err != nil {
		return contracts.Plan{}, false, fmt.Errorf("creating plan: %w", err)
	}
	if p.Version <= 0 {
		return contracts.Plan{}, false, fmt.Errorf("creating plan: version %d is not positive", p.Version)
	}
	if err := checkJSONB("plan body", p.Body); err != nil {
		return contracts.Plan{}, false, fmt.Errorf("creating plan: %w", err)
	}
	const insert = `INSERT INTO plans (feature_request_id, version, state, body)
		SELECT $1::uuid, $2::integer, 'proposed', $3::jsonb WHERE EXISTS (SELECT 1 FROM feature_requests WHERE id = $1)
		ON CONFLICT (feature_request_id, version) DO NOTHING
		RETURNING ` + planColumns
	// The body goes as text so pgx does not re-encode the bytes as a JSON
	// string.
	plan, err := scanPlan(db.QueryRow(ctx, insert, p.FeatureRequestID, p.Version, string(p.Body)))
	if err == nil {
		return plan, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return contracts.Plan{}, false, fmt.Errorf("creating plan %d of feature request %s: %w", p.Version, p.FeatureRequestID, err)
	}
	const existing = `SELECT ` + planColumns + ` FROM plans WHERE feature_request_id = $1 AND version = $2`
	plan, err = scanPlan(db.QueryRow(ctx, existing, p.FeatureRequestID, p.Version))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Plan{}, false, fmt.Errorf("creating plan %d: feature request %s: %w", p.Version, p.FeatureRequestID, ErrNotFound)
	}
	if err != nil {
		return contracts.Plan{}, false, fmt.Errorf("reading plan %d of feature request %s: %w", p.Version, p.FeatureRequestID, err)
	}
	return plan, false, nil
}

const taskColumns = `id::text, plan_id::text, spec, state, coalesce(reason, ''), revision`

func scanTask(row pgx.Row) (contracts.Task, error) {
	var t contracts.Task
	var spec []byte
	if err := row.Scan(&t.ID, &t.PlanID, &spec, &t.Status, &t.Reason, &t.Revision); err != nil {
		return contracts.Task{}, err
	}
	if err := json.Unmarshal(spec, &t.Spec); err != nil {
		return contracts.Task{}, fmt.Errorf("decoding spec of task %s: %w", t.ID, err)
	}
	return t, nil
}

// CreateTask records a task of the plan in state running and reports
// whether this call created it. The natural key is (plan, spec.Key): a
// second call returns the existing row and its stored spec unchanged. An
// unknown plan is ErrNotFound.
func CreateTask(ctx context.Context, db DBTX, planID string, spec contracts.TaskSpec) (contracts.Task, bool, error) {
	if err := checkUUID("plan ID", planID); err != nil {
		return contracts.Task{}, false, fmt.Errorf("creating task: %w", err)
	}
	if spec.Key == "" {
		return contracts.Task{}, false, fmt.Errorf("creating task in plan %s: empty task key", planID)
	}
	if err := checkText("task key", spec.Key); err != nil {
		return contracts.Task{}, false, fmt.Errorf("creating task in plan %s: %w", planID, err)
	}
	doc, err := json.Marshal(spec)
	if err != nil {
		return contracts.Task{}, false, fmt.Errorf("encoding spec of task %s: %w", spec.Key, err)
	}
	if err := checkJSONB("task spec", doc); err != nil {
		return contracts.Task{}, false, fmt.Errorf("creating task %s: %w", spec.Key, err)
	}
	const insert = `INSERT INTO tasks (plan_id, key, spec, state)
		SELECT $1::uuid, $2::text, $3::jsonb, 'running' WHERE EXISTS (SELECT 1 FROM plans WHERE id = $1)
		ON CONFLICT (plan_id, key) DO NOTHING
		RETURNING ` + taskColumns
	task, err := scanTask(db.QueryRow(ctx, insert, planID, spec.Key, string(doc)))
	if err == nil {
		return task, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return contracts.Task{}, false, fmt.Errorf("creating task %s in plan %s: %w", spec.Key, planID, err)
	}
	const existing = `SELECT ` + taskColumns + ` FROM tasks WHERE plan_id = $1 AND key = $2`
	task, err = scanTask(db.QueryRow(ctx, existing, planID, spec.Key))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Task{}, false, fmt.Errorf("creating task %s: plan %s: %w", spec.Key, planID, ErrNotFound)
	}
	if err != nil {
		return contracts.Task{}, false, fmt.Errorf("reading task %s in plan %s: %w", spec.Key, planID, err)
	}
	return task, false, nil
}
