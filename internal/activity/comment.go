package activity

import (
	"context"
	"fmt"
	"strings"
)

// PostPlanComment posts the plan on the issue so /approve is never blind.
//
// Idempotent retry, with GitHub as the record: the comment carries a
// marker naming the plan, and before posting the activity lists the
// comments this App has made on the issue and returns the one that
// carries it. A worker that dies after GitHub created the comment but
// before reporting it is retried, finds the comment, and posts nothing.
// No Postgres write follows the post, so there is no second gap. Only the
// App's own comments are searched, so a person quoting the marker cannot
// suppress the post.
func (a *Activities) PostPlanComment(ctx context.Context, in PlanCommentInput) (PlanCommentResult, error) {
	res, err := a.postPlanComment(ctx, in)
	return res, Temporal(err)
}

func (a *Activities) postPlanComment(ctx context.Context, in PlanCommentInput) (PlanCommentResult, error) {
	_, body, err := a.planOf(ctx, in.PlanID)
	if err != nil {
		return PlanCommentResult{}, err
	}
	if body.Issue.Number != in.Issue {
		return PlanCommentResult{}, fmt.Errorf("%w: plan %s is for issue %d, not %d", errNonRetryable, in.PlanID, body.Issue.Number, in.Issue)
	}
	existing, err := a.d.GitHub.ListIssueComments(ctx, in.Repo, in.Issue)
	if err != nil {
		return PlanCommentResult{}, fmt.Errorf("searching issue %d for the plan comment: %w", in.Issue, err)
	}
	marker := planMarker(in.PlanID)
	for _, c := range existing {
		if strings.Contains(c.Body, marker) {
			return PlanCommentResult{CommentID: c.ID}, nil
		}
	}
	c, err := a.d.GitHub.CreateComment(ctx, in.Repo, in.Issue, planComment(in.PlanID, body))
	if err != nil {
		return PlanCommentResult{}, fmt.Errorf("posting the plan comment on issue %d: %w", in.Issue, err)
	}
	return PlanCommentResult{CommentID: c.ID, Created: true}, nil
}
