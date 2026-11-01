package activity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/austmsk/conclave/internal/alert"
)

// AlertInput names a budget-cap stop by identifiers only: the text of the
// alert is built here from the feature request's row, so none of it
// passes through workflow history (RL-14).
type AlertInput struct {
	FeatureRequestID string
	WorkflowID       string
	// Reason is the needs_attention reason, budget_cap:<kind>.
	Reason string
	// Revision is the feature request's revision after the move, which
	// names this stop as one occurrence.
	Revision int
}

// IsBudgetCapReason reports whether a needs_attention reason is a budget
// cap stop, which raises a pause alert (FR-SEC31).
func IsBudgetCapReason(reason string) bool {
	return reason == ReasonBudgetCap || strings.HasPrefix(reason, ReasonBudgetCap+":")
}

// RaiseAlert raises the pause alert for a feature request stopped by a
// budget cap: written to the local alert log, then opened as an issue in
// the operations repository, or added to the open one for this feature
// request.
//
// Idempotent retry: the occurrence is named by the feature request and
// its revision, so a retry after a delivery whose result was lost finds
// the occurrence on the issue and posts nothing. A retry may write the
// occurrence to the local log a second time; readers group by occurrence.
func (a *Activities) RaiseAlert(ctx context.Context, in AlertInput) error {
	return Temporal(a.raiseAlert(ctx, in))
}

func (a *Activities) raiseAlert(ctx context.Context, in AlertInput) error {
	if !IsBudgetCapReason(in.Reason) {
		return fmt.Errorf("%w: reason %q raises no alert", errNonRetryable, in.Reason)
	}
	if err := checkID("feature request", in.FeatureRequestID); err != nil {
		return err
	}
	var w alert.Where
	const query = `SELECT r.full_name, f.issue_number FROM feature_requests f JOIN repos r ON r.id = f.repo_id WHERE f.id = $1`
	if err := a.d.DB.QueryRow(ctx, query, in.FeatureRequestID).Scan(&w.Repo, &w.Issue); err != nil {
		return notFound(err, "feature request", in.FeatureRequestID)
	}
	w.FeatureRequestID, w.WorkflowID = in.FeatureRequestID, in.WorkflowID
	kind, _ := strings.CutPrefix(in.Reason, ReasonBudgetCap+":")
	al := alert.BudgetCap(w, kind, int64(in.Revision), time.Now().UTC())
	if _, err := alert.Raise(ctx, a.d.AlertLog, a.d.AlertIssues, al); err != nil {
		if errors.Is(err, alert.ErrInvalidAlert) {
			err = fmt.Errorf("%w: %w", errNonRetryable, err)
		}
		return fmt.Errorf("raising the budget cap alert for %s: %w", in.FeatureRequestID, err)
	}
	return nil
}
