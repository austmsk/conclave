package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/austmsk/conclave/internal/contracts"
)

// DBTX is what the ledger and the directory need from a connection: a
// *pgxpool.Pool, a *pgx.Conn, or a pgx.Tx. The state-change helpers take a
// pgx.Tx because their audit attribution is transaction-local; these types
// sit behind router interfaces whose methods carry no transaction, so they
// hold a connection instead, and production gives them the pool.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CostLedger records model spend in cost_events and reads it back. It
// implements llm.CostLedger.
type CostLedger struct {
	db DBTX
}

// NewCostLedger returns a ledger over db, which should connect as the
// application role: cost_events is append-only to it (threat T6).
func NewCostLedger(db DBTX) *CostLedger {
	return &CostLedger{db: db}
}

// RecordCost inserts ev. A second event with the same idempotency key is
// absorbed by ON CONFLICT DO NOTHING, which needs only INSERT and SELECT,
// so a retried write succeeds and the spend is counted once (RL-2). The
// first event's values stand: a key names one provider reply, so a second
// event under it describes the same spend.
//
// An event with no OccurredAt takes the database's time.
func (l *CostLedger) RecordCost(ctx context.Context, ev contracts.CostEvent) error {
	if ev.IdempotencyKey == "" {
		return errors.New("recording cost: empty idempotency key")
	}
	if err := checkEventIDs(ev); err != nil {
		return fmt.Errorf("recording cost %s: %w", ev.IdempotencyKey, err)
	}
	var occurred *time.Time
	if !ev.OccurredAt.IsZero() {
		occurred = &ev.OccurredAt
	}
	const insert = `INSERT INTO cost_events
		(idempotency_key, repo_id, feature_request_id, attempt_id, role, provider, model,
		 input_tokens, output_tokens, cost_usd, occurred_at)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9, $10, coalesce($11, now()))
		ON CONFLICT (idempotency_key) DO NOTHING`
	_, err := l.db.Exec(ctx, insert,
		ev.IdempotencyKey, ev.RepoID, ev.FeatureRequestID, ev.AttemptID, string(ev.Role), ev.Provider, ev.Model,
		ev.InputTokens, ev.OutputTokens, ev.CostUSD, occurred)
	if isInvalidText(err) {
		return fmt.Errorf("recording cost %s: repository, feature request, or attempt ID: %w: %w", ev.IdempotencyKey, ErrInvalidID, err)
	}
	if err != nil {
		return fmt.Errorf("recording cost %s: %w", ev.IdempotencyKey, err)
	}
	return nil
}

// checkEventIDs requires a repository ID and allows the feature request and
// attempt to be absent, but every ID present must be a UUID.
func checkEventIDs(ev contracts.CostEvent) error {
	if err := checkUUID("repository ID", ev.RepoID); err != nil {
		return err
	}
	if ev.FeatureRequestID != "" {
		if err := checkUUID("feature request ID", ev.FeatureRequestID); err != nil {
			return err
		}
	}
	if ev.AttemptID != "" {
		return checkUUID("attempt ID", ev.AttemptID)
	}
	return nil
}

// AttemptTokens is the input plus output tokens recorded for an attempt,
// zero when nothing has been.
func (l *CostLedger) AttemptTokens(ctx context.Context, attemptID string) (int, error) {
	if err := checkUUID("attempt ID", attemptID); err != nil {
		return 0, err
	}
	const sum = `SELECT coalesce(sum(input_tokens + output_tokens), 0)::bigint
		FROM cost_events WHERE attempt_id = $1`
	var total int64
	err := l.db.QueryRow(ctx, sum, attemptID).Scan(&total)
	if isInvalidText(err) {
		return 0, fmt.Errorf("summing cost for attempt %q: %w: %w", attemptID, ErrInvalidID, err)
	}
	if err != nil {
		return 0, fmt.Errorf("summing cost for attempt %s: %w", attemptID, err)
	}
	return int(total), nil
}
