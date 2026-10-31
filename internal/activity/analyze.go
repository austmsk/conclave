package activity

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/store"
)

// PlanVersion is the only plan version Milestone 1 creates; /revise adds
// versions in Milestone 4.
const PlanVersion = 1

// Analyze records the feature request and, for an issue the owner wrote,
// parses its Task, Acceptance criteria and Hints sections into plan
// version 1, with no model call. An issue someone else wrote is not
// planned from: its whole body would be untrusted text, so the result
// carries a Problem asking the owner to write the task, and the workflow
// stops in needs_attention.
//
// Idempotent retry: every write is a store create keyed on its natural key
// ((repository, issue) and (feature request, version)) in one
// transaction, so a retry after a commit whose result was lost returns the
// rows the first call made. There is no outside action.
func (a *Activities) Analyze(ctx context.Context, in AnalyzeInput) (AnalyzeResult, error) {
	res, err := a.analyze(ctx, in)
	return res, Temporal(err)
}

func (a *Activities) analyze(ctx context.Context, in AnalyzeInput) (AnalyzeResult, error) {
	cfg, err := a.repo(in.Repo.FullName)
	if err != nil {
		return AnalyzeResult{}, err
	}
	repo, found, err := a.repos.RepoByFullName(ctx, in.Repo.FullName)
	if err != nil {
		return AnalyzeResult{}, err
	}
	if !found {
		return AnalyzeResult{}, fmt.Errorf("%w: repository %s has no row; the worker seeds it at startup", errNonRetryable, in.Repo.FullName)
	}
	body, problem, err := a.planBody(in, cfg)
	if err != nil {
		return AnalyzeResult{}, err
	}
	var res AnalyzeResult
	err = pgx.BeginFunc(ctx, a.d.DB, func(tx pgx.Tx) error {
		res, err = a.recordAnalysis(ctx, tx, in, repo.ID, body)
		return err
	})
	if err != nil {
		return AnalyzeResult{}, fmt.Errorf("recording analysis of issue %d: %w", in.Issue.Number, err)
	}
	res.Problem = problem
	return res, nil
}

// planBody builds the plan body, or returns the problem that stops the
// issue from being planned. Only the owner's own account counts as the
// owner: the source is decided from the author fields alone (FR-SEC28).
func (a *Activities) planBody(in AnalyzeInput, cfg Repo) ([]byte, string, error) {
	if github.Classify(in.Issue.Author, a.d.OwnerUserID, 0) != github.SourceOwner {
		return nil, "the issue was not written by the owner; the owner must write the Task and Acceptance criteria", nil
	}
	spec, problem := ParseIssue(in.Issue.Body)
	if problem != "" {
		return nil, problem, nil
	}
	q, err := github.NewQuarantine()
	if err != nil {
		return nil, "", err
	}
	body := PlanBody{
		Task:          spec,
		IssueText:     q.Wrap(github.SourceOwner, in.Issue.Author, in.Issue.Body),
		Profile:       cfg.Toolchain.Profile,
		Constraints:   cfg.Gates.Constraints(),
		ConfigDigests: a.d.ConfigDigests,
		Issue:         PlanIssue{Number: in.Issue.Number, Title: in.Issue.Title, AuthorID: in.Issue.Author.ID},
		Repository:    PlanRepository{FullName: in.Repo.FullName, GitHubID: in.Repo.ID},
	}
	doc, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("encoding plan body: %w", err)
	}
	return doc, "", nil
}

// recordAnalysis creates the feature request and, when body is set, the
// plan. Both creates return the existing row on a repeat, so the body a
// retry computes, with its fresh quarantine nonce, never replaces the
// stored one.
func (a *Activities) recordAnalysis(ctx context.Context, tx pgx.Tx, in AnalyzeInput, repoID string, body []byte) (AnalyzeResult, error) {
	fr, _, err := store.CreateFeatureRequest(ctx, tx, store.FeatureRequestParams{
		RepoID: repoID, IssueNumber: in.Issue.Number, Title: in.Issue.Title, WorkflowID: in.WorkflowID,
	})
	if err != nil {
		return AnalyzeResult{}, err
	}
	res := AnalyzeResult{FeatureRequestID: fr.ID, RepoID: repoID, Status: fr.Status, Revision: fr.Revision}
	if body == nil {
		return res, nil
	}
	plan, _, err := store.CreatePlan(ctx, tx, store.PlanParams{FeatureRequestID: fr.ID, Version: PlanVersion, Body: body})
	if err != nil {
		return AnalyzeResult{}, err
	}
	res.PlanID, res.PlanRevision, res.PlanStatus = plan.ID, plan.Revision, plan.Status
	return res, nil
}

// planOf reads and decodes a plan's body.
func (a *Activities) planOf(ctx context.Context, planID string) (planRow, PlanBody, error) {
	row, err := readPlan(ctx, a.d.DB, planID)
	if err != nil {
		return planRow{}, PlanBody{}, err
	}
	var body PlanBody
	if err := json.Unmarshal(row.Body, &body); err != nil {
		return planRow{}, PlanBody{}, fmt.Errorf("%w: decoding body of plan %s: %v", errNonRetryable, planID, err)
	}
	return row, body, nil
}
