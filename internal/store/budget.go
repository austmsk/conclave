package store

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/llm"
)

// budgetLockKey is the transaction-level advisory lock every reservation
// takes. There is one key for every cap and every feature request: the
// platform has one owner, so contention is negligible, and a single lock
// cannot be taken in two orders. The value is "conclave" in ASCII.
const budgetLockKey int64 = 0x636f6e636c617665

// Attributed reports whether the feature request belongs to the repository
// and, when attemptID is not empty, the attempt to the feature request
// through its task and plan. An ID that is not a UUID cannot belong to
// anything and reports false.
func (l *CostLedger) Attributed(ctx context.Context, repoID, featureRequestID, attemptID string) (bool, error) {
	if checkUUID("repository ID", repoID) != nil || checkUUID("feature request ID", featureRequestID) != nil {
		return false, nil
	}
	if attemptID != "" && checkUUID("attempt ID", attemptID) != nil {
		return false, nil
	}
	const query = `SELECT EXISTS (
		SELECT 1 FROM feature_requests f
		WHERE f.id = $1 AND f.repo_id = $2
		  AND ($3::text = '' OR EXISTS (
		      SELECT 1 FROM attempts a
		      JOIN tasks t ON t.id = a.task_id
		      JOIN plans p ON p.id = t.plan_id
		      WHERE a.id = NULLIF($3::text, '')::uuid AND p.feature_request_id = f.id)))`
	var ok bool
	if err := l.db.QueryRow(ctx, query, featureRequestID, repoID, attemptID).Scan(&ok); err != nil {
		return false, fmt.Errorf("checking attempt %q is in feature request %s: %w", attemptID, featureRequestID, err)
	}
	return ok, nil
}

// Reserve runs one short transaction: it takes the budget lock, reads the
// spend settled and reserved against each cap, asks decide for the claim, and inserts it as a reservation that
// expires ttl after now on the database clock. While the lock is held no
// other Reserve can read the same headroom, and a transaction that held it
// before has committed, so its reservation is visible here. decide's error
// is returned unchanged, and nothing is reserved.
//
// That visibility holds only at READ COMMITTED, where each statement takes
// a fresh snapshot. At REPEATABLE READ or SERIALIZABLE the snapshot is taken
// by the transaction's first statement, before the lock is granted, so a
// Reserve that waited on the lock would read the spend as it was before
// the previous holder committed, and two calls would be granted the same
// headroom. Reserve therefore opens its transaction at READ COMMITTED
// whatever the server's default, and refuses to run at any other level,
// which it can meet when db is a transaction it can only nest inside.
func (l *CostLedger) Reserve(ctx context.Context, attemptID, featureRequestID string, ttl time.Duration, decide func(llm.Spend) (llm.Claim, error)) (string, error) {
	if err := checkSpendIDs(attemptID, featureRequestID); err != nil {
		return "", fmt.Errorf("reserving budget: %w", err)
	}
	if ttl <= 0 {
		return "", fmt.Errorf("reserving budget: reservation would expire at once (ttl %s)", ttl)
	}
	var id string
	var refused error
	err := l.readCommitted(ctx, func(tx pgx.Tx) error {
		if err := lockBudgets(ctx, tx); err != nil {
			return err
		}
		sp, err := readSpend(ctx, tx, attemptID, featureRequestID)
		if err != nil {
			return err
		}
		claim, err := decide(sp)
		if err != nil {
			refused = err
			return err
		}
		id, err = insertReservation(ctx, tx, attemptID, featureRequestID, claim, ttl)
		return err
	})
	if refused != nil {
		return "", refused
	}
	if err != nil {
		return "", fmt.Errorf("reserving budget for feature request %s: %w", featureRequestID, err)
	}
	return id, nil
}

// Spend reports the spend settled and reserved against each cap for the
// attempt (none when attemptID is empty), the feature request, and the
// current UTC month, and reserves nothing. It takes no lock, so the figures
// may be stale by the time the caller acts on them: admitting a model call
// goes through Reserve, never through this.
func (l *CostLedger) Spend(ctx context.Context, attemptID, featureRequestID string) (llm.Spend, error) {
	if err := checkSpendIDs(attemptID, featureRequestID); err != nil {
		return llm.Spend{}, fmt.Errorf("reading spend: %w", err)
	}
	sp, err := readSpend(ctx, l.db, attemptID, featureRequestID)
	if err != nil {
		return llm.Spend{}, fmt.Errorf("reading spend for feature request %s: %w", featureRequestID, err)
	}
	return sp, nil
}

func checkSpendIDs(attemptID, featureRequestID string) error {
	if attemptID != "" {
		if err := checkUUID("attempt ID", attemptID); err != nil {
			return err
		}
	}
	return checkUUID("feature request ID", featureRequestID)
}

// readCommitted runs fn in a READ COMMITTED transaction. A pool or a
// connection opens one at that level. A transaction can only be nested
// inside, as a savepoint at the outer level, so fn must check the level
// itself; lockBudgets does.
func (l *CostLedger) readCommitted(ctx context.Context, fn func(pgx.Tx) error) error {
	type txBeginner interface {
		BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	}
	if b, ok := l.db.(txBeginner); ok {
		return pgx.BeginTxFunc(ctx, b, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
	}
	return pgx.BeginFunc(ctx, l.db, fn)
}

// lockBudgets refuses a transaction that is not at READ COMMITTED and takes
// the budget lock.
//
// Nothing under the lock may wait on another transaction, or every model
// call queues behind it. That is why expired reservations are not deleted
// here: a Settle that has deleted one and not yet committed holds its row,
// and the delete would wait for it. Skipping locked rows needs a locking
// clause, which needs UPDATE, which the application role does not have.
// Expired rows are left out of every sum instead, and the expires_at index
// keeps those sums to the live rows.
func lockBudgets(ctx context.Context, tx pgx.Tx) error {
	var level string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&level); err != nil {
		return fmt.Errorf("reading the isolation level: %w", err)
	}
	if level != "read committed" {
		return fmt.Errorf("budget reservations need read committed isolation, not %s: under the lock a %s snapshot cannot see the previous reservation", level, level)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, budgetLockKey); err != nil {
		return fmt.Errorf("taking the budget lock: %w", err)
	}
	return nil
}

// readSpend sums settled and live reserved spend for the attempt, the
// feature request, and the current UTC calendar month. It is one statement
// so that every sum comes from one snapshot: read in two, a Settle
// committing between them could move a call from reserved to recorded
// after the events were summed and before the reservations were, and the
// call would be counted nowhere.
//
// The month starts at midnight UTC on the first, computed in SQL because
// the router reads no clock. date_trunc of a timestamptz truncates in the
// session's time zone, so the time is first converted to UTC wall time and
// the result converted back. Live reservations all belong to the current
// month. statement_timestamp is used rather than now(), which is when the
// transaction began, possibly before a wait for the lock.
func readSpend(ctx context.Context, db DBTX, attemptID, featureRequestID string) (llm.Spend, error) {
	const query = `SELECT
		(SELECT coalesce(sum(input_tokens + output_tokens), 0) FROM cost_events
		  WHERE attempt_id = NULLIF($1, '')::uuid)
		+ (SELECT coalesce(sum(tokens), 0) FROM budget_reservations
		  WHERE attempt_id = NULLIF($1, '')::uuid AND expires_at > statement_timestamp()),
		(SELECT coalesce(sum(input_tokens + output_tokens), 0) FROM cost_events
		  WHERE feature_request_id = $2)
		+ (SELECT coalesce(sum(tokens), 0) FROM budget_reservations
		  WHERE feature_request_id = $2 AND expires_at > statement_timestamp()),
		(SELECT coalesce(sum(cost_usd), 0) FROM cost_events
		  WHERE occurred_at >= date_trunc('month', statement_timestamp() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')
		+ (SELECT coalesce(sum(cost_usd), 0) FROM budget_reservations
		  WHERE expires_at > statement_timestamp())`
	var attempt, feature int64
	var month float64
	err := db.QueryRow(ctx, query, attemptID, featureRequestID).Scan(&attempt, &feature, &month)
	if err != nil {
		return llm.Spend{}, fmt.Errorf("summing spend: %w", err)
	}
	return llm.Spend{AttemptTokens: int(attempt), FeatureRequestTokens: int(feature), MonthUSD: month}, nil
}

// insertReservation stores a claim. Its price is rounded up to the
// column's six decimal places, so rounding never makes room under the
// monthly cap. A claim that is negative or not a number is refused:
// either would make room under a cap, or make every comparison with the
// cap false.
func insertReservation(ctx context.Context, tx pgx.Tx, attemptID, featureRequestID string, c llm.Claim, ttl time.Duration) (string, error) {
	if c.Tokens < 0 || !(c.CostUSD >= 0) || math.IsInf(c.CostUSD, 0) {
		return "", fmt.Errorf("refusing reservation of %d tokens and $%v", c.Tokens, c.CostUSD)
	}
	const insert = `INSERT INTO budget_reservations (attempt_id, feature_request_id, tokens, cost_usd, expires_at)
		VALUES (NULLIF($1, '')::uuid, $2, $3, ceil($4::numeric * 1000000) / 1000000,
		        statement_timestamp() + $5::bigint * interval '1 microsecond')
		RETURNING id::text`
	var id string
	err := tx.QueryRow(ctx, insert, attemptID, featureRequestID, c.Tokens, c.CostUSD, ttl.Microseconds()).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("inserting reservation: %w", err)
	}
	return id, nil
}
