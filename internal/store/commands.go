package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Command outcomes, as the commands table stores them.
const (
	CommandAccepted = "accepted"
	CommandRejected = "rejected"
)

// commandNames are the command words the commands table accepts. They
// match github.CommandName; store does not import github.
var commandNames = []string{"approve", "revise", "ask", "cancel", "status", "retry", "help"}

// ErrInvalidCommandRecord means a command record is missing a field the
// audit needs or carries a value the table would refuse.
var ErrInvalidCommandRecord = errors.New("invalid command record")

// CommandRecord is one owner command and what was decided about it
// (FR-SEC10). FeatureRequestID and StatusAtDecision are empty when the
// issue had no feature request yet; Reason is set exactly when the command
// was rejected.
type CommandRecord struct {
	CommentID        int64
	DeliveryID       string
	WorkflowID       string
	RepoGitHubID     int64
	IssueNumber      int
	FeatureRequestID string
	Command          string
	Actor            string
	StatusAtDecision string
	Outcome          string
	Reason           string
}

// RecordCommand appends rec to the commands audit and reports whether it
// was inserted. A second record for the same comment ID is absorbed by ON
// CONFLICT DO NOTHING, which needs only INSERT and SELECT, so a retried
// write records the command once (RL-2) and the first decision stands.
func RecordCommand(ctx context.Context, db DBTX, rec CommandRecord) (bool, error) {
	if err := checkCommandRecord(rec); err != nil {
		return false, err
	}
	const query = `INSERT INTO commands
		(comment_id, delivery_id, workflow_id, repo_github_id, issue_number, feature_request_id,
		 command, actor, status_at_decision, outcome, reason)
		VALUES ($1, $2, $3, $4, $5, $6::uuid, $7, $8, $9, $10, $11)
		ON CONFLICT (comment_id) DO NOTHING`
	tag, err := db.Exec(ctx, query, rec.CommentID, rec.DeliveryID, rec.WorkflowID, rec.RepoGitHubID, rec.IssueNumber,
		nullable(rec.FeatureRequestID), rec.Command, rec.Actor, nullable(rec.StatusAtDecision), rec.Outcome, nullable(rec.Reason))
	if err != nil {
		return false, fmt.Errorf("recording command %d: %w", rec.CommentID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// CommandRecorded reports whether the command with this comment ID is
// already in the audit.
func CommandRecorded(ctx context.Context, db DBTX, commentID int64) (bool, error) {
	var found bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM commands WHERE comment_id = $1)`, commentID).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("looking up command %d: %w", commentID, err)
	}
	return found, nil
}

func checkCommandRecord(rec CommandRecord) error {
	switch {
	case rec.CommentID <= 0 || rec.RepoGitHubID <= 0 || rec.IssueNumber <= 0:
		return fmt.Errorf("%w: comment %d, repository %d, issue %d must all be positive",
			ErrInvalidCommandRecord, rec.CommentID, rec.RepoGitHubID, rec.IssueNumber)
	case rec.WorkflowID == "":
		return fmt.Errorf("%w: comment %d has no workflow ID", ErrInvalidCommandRecord, rec.CommentID)
	case !slices.Contains(commandNames, rec.Command):
		return fmt.Errorf("%w: comment %d: unknown command %q", ErrInvalidCommandRecord, rec.CommentID, rec.Command)
	case !strings.HasPrefix(rec.Actor, "github:"):
		return fmt.Errorf("%w: comment %d: actor %q is not a GitHub user", ErrInvalidCommandRecord, rec.CommentID, rec.Actor)
	case rec.Outcome != CommandAccepted && rec.Outcome != CommandRejected:
		return fmt.Errorf("%w: comment %d: outcome %q", ErrInvalidCommandRecord, rec.CommentID, rec.Outcome)
	case (rec.Outcome == CommandRejected) != (rec.Reason != ""):
		return fmt.Errorf("%w: comment %d: a reason is required exactly when a command is rejected", ErrInvalidCommandRecord, rec.CommentID)
	}
	if rec.FeatureRequestID != "" {
		return checkUUID("feature request ID", rec.FeatureRequestID)
	}
	return nil
}

// nullable maps an empty string to SQL NULL.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
