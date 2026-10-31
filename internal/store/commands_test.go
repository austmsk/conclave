package store_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/store"
)

// TestCommandsSchemaChecks proves the commands table refuses what the
// audit must not hold, at the database, whatever the Go code sends.
func TestCommandsSchemaChecks(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	const insert = `INSERT INTO commands
		(comment_id, delivery_id, workflow_id, repo_github_id, issue_number, feature_request_id,
		 command, actor, status_at_decision, outcome, reason)
		VALUES ($1, 'd', 'feature/1/1', 1, 1, $2, $3, $4, $5, $6, $7)`
	tests := []struct {
		name                            string
		command, actor, status, outcome string
		reason                          any
		want                            string
	}{
		{"accepted", "approve", "github:1", "awaiting_approval", "accepted", nil, ""},
		{"rejected with a reason", "retry", "github:1", "needs_attention", "rejected", "over_cap:month", ""},
		{"unknown command", "resume", "github:1", "needs_attention", "rejected", "x", checkViolation},
		{"actor not a GitHub user", "help", "workflow:feature/1/1", "planning", "accepted", nil, checkViolation},
		{"unknown status", "help", "github:1", "paused", "accepted", nil, checkViolation},
		{"unknown outcome", "help", "github:1", "planning", "ignored", nil, checkViolation},
		{"rejected without a reason", "help", "github:1", "planning", "rejected", nil, checkViolation},
		{"accepted with a reason", "help", "github:1", "planning", "accepted", "x", checkViolation},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := savepoint(t, ctx, tx)
			_, err := sp.Exec(ctx, insert, int64(1000+i), s.feature, tt.command, tt.actor, tt.status, tt.outcome, tt.reason)
			if got := sqlState(err); got != tt.want {
				t.Fatalf("got error %v (SQLSTATE %q), want SQLSTATE %q", err, got, tt.want)
			}
		})
	}
}

// TestRecordCommandOncePerComment records the same comment twice through
// the store as the application role: the second is absorbed, not doubled
// and not an error.
func TestRecordCommandOncePerComment(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	rec := store.CommandRecord{
		CommentID: 77, DeliveryID: "d", WorkflowID: "feature/1/1", RepoGitHubID: 1, IssueNumber: 1,
		FeatureRequestID: s.feature, Command: "cancel", Actor: "github:1", StatusAtDecision: "planning", Outcome: store.CommandAccepted,
	}
	var inserted []bool
	err := asAppRole(t, ctx, tx, func(sp pgx.Tx) error {
		for range 2 {
			ok, err := store.RecordCommand(ctx, sp, rec)
			if err != nil {
				return err
			}
			inserted = append(inserted, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inserted) != 2 || !inserted[0] || inserted[1] {
		t.Fatalf("inserted = %v, want the first only", inserted)
	}
}
