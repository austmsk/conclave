package activity

import (
	"context"
	"fmt"
	"strings"

	"github.com/austmsk/conclave/internal/github"
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
	id, found, err := a.findMarked(ctx, in.Repo, in.Issue, planMarker(in.PlanID))
	if err != nil {
		return PlanCommentResult{}, fmt.Errorf("searching issue %d for the plan comment: %w", in.Issue, err)
	}
	if found {
		return PlanCommentResult{CommentID: id}, nil
	}
	c, err := a.d.GitHub.CreateComment(ctx, in.Repo, in.Issue, planComment(in.PlanID, body))
	if err != nil {
		return PlanCommentResult{}, fmt.Errorf("posting the plan comment on issue %d: %w", in.Issue, err)
	}
	return PlanCommentResult{CommentID: c.ID, Created: true}, nil
}

// findMarked returns the comment this App posted on the issue that
// carries marker. Only the App's own comments are listed, so a person
// quoting a marker cannot make the platform skip a post or edit their
// comment.
func (a *Activities) findMarked(ctx context.Context, repo github.RepoRef, issue int, marker string) (int64, bool, error) {
	existing, err := a.d.GitHub.ListIssueComments(ctx, repo, issue)
	if err != nil {
		return 0, false, err
	}
	for _, c := range existing {
		if strings.Contains(c.Body, marker) {
			return c.ID, true, nil
		}
	}
	return 0, false, nil
}

// UpsertStatusComment keeps one status comment on the issue, edited in
// place as the feature request moves: the status, the reason and the cap
// when there is one, the phase it left, and the commands available
// (FR-S12).
//
// Idempotent retry, with GitHub as the record, as PostPlanComment: the
// comment carries a marker naming the feature request, and the activity
// edits the App's comment that carries it, or creates it when there is
// none. A worker that dies after GitHub created the comment is retried,
// finds it, and edits it to the same body. Editing twice leaves one
// comment with the last body. No Postgres write follows.
func (a *Activities) UpsertStatusComment(ctx context.Context, in StatusCommentInput) error {
	return Temporal(a.upsertStatusComment(ctx, in))
}

func (a *Activities) upsertStatusComment(ctx context.Context, in StatusCommentInput) error {
	if err := checkID("feature request", in.View.FeatureRequestID); err != nil {
		return err
	}
	body := statusComment(in.Issue, in.View)
	id, found, err := a.findMarked(ctx, in.Repo, in.Issue, statusMarker(in.View.FeatureRequestID))
	if err != nil {
		return fmt.Errorf("searching issue %d for the status comment: %w", in.Issue, err)
	}
	if found {
		if err := a.d.GitHub.EditComment(ctx, in.Repo, id, body); err != nil {
			return fmt.Errorf("editing status comment %d on issue %d: %w", id, in.Issue, err)
		}
		return nil
	}
	if _, err := a.d.GitHub.CreateComment(ctx, in.Repo, in.Issue, body); err != nil {
		return fmt.Errorf("posting the status comment on issue %d: %w", in.Issue, err)
	}
	return nil
}

// ReplyToCommand answers one command with a new comment: why it was
// rejected, or what /status and /help report.
//
// Idempotent retry: the reply carries a marker naming the command's
// comment ID, and an existing reply carrying it is left alone, so each
// command gets one reply however often the activity runs. No Postgres
// write follows.
func (a *Activities) ReplyToCommand(ctx context.Context, in ReplyInput) error {
	return Temporal(a.replyToCommand(ctx, in))
}

func (a *Activities) replyToCommand(ctx context.Context, in ReplyInput) error {
	if in.Ref.CommentID <= 0 || in.Ref.Issue <= 0 {
		return fmt.Errorf("%w: reply to comment %d on issue %d", errNonRetryable, in.Ref.CommentID, in.Ref.Issue)
	}
	_, found, err := a.findMarked(ctx, in.Ref.Repo, in.Ref.Issue, replyMarker(in.Ref.CommentID))
	if err != nil {
		return fmt.Errorf("searching issue %d for the reply to comment %d: %w", in.Ref.Issue, in.Ref.CommentID, err)
	}
	if found {
		return nil
	}
	if _, err := a.d.GitHub.CreateComment(ctx, in.Ref.Repo, in.Ref.Issue, replyBody(in)); err != nil {
		return fmt.Errorf("replying to comment %d on issue %d: %w", in.Ref.CommentID, in.Ref.Issue, err)
	}
	return nil
}
