package activity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/store"
)

// RecordCommand appends one owner command, accepted or rejected, to the
// commands audit with its actor, the status it was decided in, and why it
// was rejected (FR-SEC10).
//
// Idempotent retry: the row is keyed on the command's comment ID with ON
// CONFLICT DO NOTHING, so a retry after a commit whose result was lost,
// or a second delivery of the same comment, records the command once.
// There is no outside action.
func (a *Activities) RecordCommand(ctx context.Context, in RecordCommandInput) error {
	return Temporal(a.recordCommand(ctx, in))
}

func (a *Activities) recordCommand(ctx context.Context, in RecordCommandInput) error {
	_, err := store.RecordCommand(ctx, a.d.DB, store.CommandRecord{
		CommentID: in.Ref.CommentID, DeliveryID: in.Ref.DeliveryID, WorkflowID: in.Ref.WorkflowID,
		RepoGitHubID: in.Ref.Repo.ID, IssueNumber: in.Ref.Issue, FeatureRequestID: in.FeatureRequestID,
		Command: string(in.Ref.Command), Actor: store.ActorGitHub(in.Ref.ActorID),
		StatusAtDecision: string(in.Status), Outcome: in.Outcome, Reason: in.Reason,
	})
	return err
}

// LookupFeature finds the feature request recorded under a feature
// workflow ID, for a command that arrived when no workflow was running,
// and reports which workflow recorded the command, if one has: its live
// run, when it is delivered again after the run closed, or an earlier
// try of the same closed-run workflow.
//
// Idempotent retry: it only reads.
func (a *Activities) LookupFeature(ctx context.Context, in LookupFeatureInput) (LookupFeatureResult, error) {
	res, err := a.lookupFeature(ctx, in)
	return res, Temporal(err)
}

func (a *Activities) lookupFeature(ctx context.Context, in LookupFeatureInput) (LookupFeatureResult, error) {
	if in.WorkflowID == "" {
		return LookupFeatureResult{}, fmt.Errorf("%w: no workflow ID to look up", errNonRetryable)
	}
	var res LookupFeatureResult
	by, err := store.CommandRecordedBy(ctx, a.d.DB, in.CommentID)
	if err != nil {
		return LookupFeatureResult{}, err
	}
	res.RecordedBy = by
	const query = `SELECT id::text, state, coalesce(reason, '') FROM feature_requests
		WHERE workflow_id = $1 ORDER BY created_at, id LIMIT 1`
	err = a.d.DB.QueryRow(ctx, query, in.WorkflowID).Scan(&res.FeatureRequestID, &res.Status, &res.Reason)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return res, nil
	case err != nil:
		return LookupFeatureResult{}, fmt.Errorf("looking up the feature request of %s: %w", in.WorkflowID, err)
	}
	res.Found = true
	return res, nil
}
