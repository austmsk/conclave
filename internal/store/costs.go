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

// TxDB is a DBTX that can also open a transaction: a *pgxpool.Pool, a
// *pgx.Conn, or a pgx.Tx, whose Begin opens a savepoint. The ledger needs
// one, because reserving and settling each take several statements that
// must commit together.
type TxDB interface {
	DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// CostLedger records model spend in cost_events, reserves spend for calls
// in flight in budget_reservations, and enforces the FR-SEC9 caps over
// both. It implements llm.CostLedger.
type CostLedger struct {
	db TxDB
}

// NewCostLedger returns a ledger over db, which should connect as the
// application role: cost_events is append-only to it (threat T6). In
// production db is the pool; the caps hold across connections and
// processes because every reservation takes the same transaction lock.
func NewCostLedger(db TxDB) *CostLedger {
	return &CostLedger{db: db}
}

// Settle inserts ev and deletes its reservation in one transaction, so the
// spend moves from reserved to recorded without a moment in which it is
// counted in neither. A second event with the same idempotency key is
// absorbed by ON CONFLICT DO NOTHING, which needs only INSERT and SELECT,
// so a retried Settle succeeds and the spend is counted once (RL-2). The
// first event's values stand: a key names one provider reply, so a second
// event under it describes the same spend. A reservation already gone,
// settled by an earlier try or never taken, is not an error. An empty
// reservationID records spend that was never reserved.
//
// An event with no OccurredAt takes the database's time.
func (l *CostLedger) Settle(ctx context.Context, reservationID string, ev contracts.CostEvent) error {
	if ev.IdempotencyKey == "" {
		return errors.New("recording cost: empty idempotency key")
	}
	if err := checkEventIDs(ev); err != nil {
		return fmt.Errorf("recording cost %s: %w", ev.IdempotencyKey, err)
	}
	if reservationID != "" {
		if err := checkUUID("reservation ID", reservationID); err != nil {
			return fmt.Errorf("recording cost %s: %w", ev.IdempotencyKey, err)
		}
	}
	err := pgx.BeginFunc(ctx, l.db, func(tx pgx.Tx) error {
		if err := insertCost(ctx, tx, ev); err != nil {
			return err
		}
		if reservationID == "" {
			return nil
		}
		return deleteReservation(ctx, tx, reservationID)
	})
	if isInvalidText(err) {
		return fmt.Errorf("recording cost %s: repository, feature request, or attempt ID: %w: %w", ev.IdempotencyKey, ErrInvalidID, err)
	}
	if err != nil {
		return fmt.Errorf("recording cost %s: %w", ev.IdempotencyKey, err)
	}
	return nil
}

// Release deletes a reservation whose call billed nothing. Releasing one
// already gone is not an error.
func (l *CostLedger) Release(ctx context.Context, reservationID string) error {
	if err := checkUUID("reservation ID", reservationID); err != nil {
		return fmt.Errorf("releasing reservation: %w", err)
	}
	if err := deleteReservation(ctx, l.db, reservationID); err != nil {
		return fmt.Errorf("releasing reservation %s: %w", reservationID, err)
	}
	return nil
}

func insertCost(ctx context.Context, db DBTX, ev contracts.CostEvent) error {
	var occurred *time.Time
	if !ev.OccurredAt.IsZero() {
		occurred = &ev.OccurredAt
	}
	const insert = `INSERT INTO cost_events
		(idempotency_key, repo_id, feature_request_id, attempt_id, role, provider, model,
		 input_tokens, output_tokens, cost_usd, occurred_at)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9, $10, coalesce($11, now()))
		ON CONFLICT (idempotency_key) DO NOTHING`
	_, err := db.Exec(ctx, insert,
		ev.IdempotencyKey, ev.RepoID, ev.FeatureRequestID, ev.AttemptID, string(ev.Role), ev.Provider, ev.Model,
		ev.InputTokens, ev.OutputTokens, ev.CostUSD, occurred)
	return err
}

func deleteReservation(ctx context.Context, db DBTX, id string) error {
	_, err := db.Exec(ctx, `DELETE FROM budget_reservations WHERE id = $1`, id)
	return err
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
