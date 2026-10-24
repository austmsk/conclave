package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
)

// QueryTxDB is a TxDB that can also return rows: a *pgxpool.Pool, a
// *pgx.Conn, or a pgx.Tx.
type QueryTxDB interface {
	TxDB
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// GateResults records gate results in gate_results, which the application
// role may insert and read but not update (migration 00007). It implements
// gate.Results; the assertion lives in gates_test.go so store does not
// import gate.
type GateResults struct {
	db QueryTxDB
}

// NewGateResults returns a recorder over db, which should connect as the
// application role so a recorded result cannot be rewritten through it.
// In production db is the pool.
func NewGateResults(db QueryTxDB) *GateResults {
	return &GateResults{db: db}
}

// artifactHashRE is the CHECK on gate_results.artifact_hash, applied
// before any SQL runs.
var artifactHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

const gateColumns = `id::text, attempt_id::text, gate, passed, flagged, artifact_hash, mode,
	detail, coalesce(artifact_key, ''), ran_at, duration_ms`

// Record inserts every result in one transaction and returns the rows as
// stored, ordered by gate. All results must share one attempt and artifact
// hash. A row already stored under the same (attempt, gate, artifact hash)
// is kept: ON CONFLICT DO NOTHING needs only INSERT, so a retried write
// succeeds, and because the stored rows are what is returned, a retry that
// computed a different verdict gets the first one back (RL-2).
//
// Duration is stored in whole milliseconds and RanAt at microsecond
// precision; the returned rows carry the stored values.
func (r *GateResults) Record(ctx context.Context, results []contracts.GateResult) ([]contracts.GateResult, error) {
	if err := checkGateBatch(results); err != nil {
		return nil, fmt.Errorf("recording gate results: %w", err)
	}
	attemptID, hash := results[0].AttemptID, results[0].ArtifactHash
	gates := make([]string, len(results))
	for i, res := range results {
		gates[i] = string(res.Gate)
	}
	var stored []contracts.GateResult
	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		for _, res := range results {
			if err := insertGateResult(ctx, tx, res); err != nil {
				return fmt.Errorf("inserting %s: %w", res.Gate, err)
			}
		}
		var err error
		stored, err = queryGateResults(ctx, tx,
			`WHERE attempt_id = $1 AND artifact_hash = $2 AND gate = ANY($3)`, attemptID, hash, gates)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("recording gate results for attempt %s artifact %s: %w", attemptID, hash, err)
	}
	if len(stored) != len(results) {
		return nil, fmt.Errorf("recording gate results for attempt %s artifact %s: read back %d rows, wrote %d",
			attemptID, hash, len(stored), len(results))
	}
	return stored, nil
}

// ForArtifact returns every result recorded for one attempt and artifact
// hash, ordered by gate.
func (r *GateResults) ForArtifact(ctx context.Context, attemptID, artifactHash string) ([]contracts.GateResult, error) {
	if err := checkUUID("attempt ID", attemptID); err != nil {
		return nil, fmt.Errorf("reading gate results: %w", err)
	}
	if !artifactHashRE.MatchString(artifactHash) {
		return nil, fmt.Errorf("reading gate results: artifact hash %q is not lowercase hex SHA-256", artifactHash)
	}
	rows, err := queryGateResults(ctx, r.db, `WHERE attempt_id = $1 AND artifact_hash = $2`, attemptID, artifactHash)
	if err != nil {
		return nil, fmt.Errorf("reading gate results for attempt %s artifact %s: %w", attemptID, artifactHash, err)
	}
	return rows, nil
}

func insertGateResult(ctx context.Context, db DBTX, res contracts.GateResult) error {
	const insert = `INSERT INTO gate_results
		(attempt_id, gate, passed, flagged, artifact_hash, mode, detail, artifact_key, ran_at, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10)
		ON CONFLICT (attempt_id, gate, artifact_hash) DO NOTHING`
	_, err := db.Exec(ctx, insert,
		res.AttemptID, string(res.Gate), res.Passed, res.Flagged, res.ArtifactHash, string(res.Mode),
		res.Detail, res.ArtifactKey, res.RanAt.Truncate(time.Microsecond), res.Duration.Milliseconds())
	return err
}

// queryGateResults runs a select over gate_results with the given WHERE
// clause, ordered by gate.
func queryGateResults(ctx context.Context, db QueryTxDB, where string, args ...any) ([]contracts.GateResult, error) {
	rows, err := db.Query(ctx, `SELECT `+gateColumns+` FROM gate_results `+where+` ORDER BY gate`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.GateResult
	for rows.Next() {
		var g contracts.GateResult
		var durationMS int64
		if err := rows.Scan(&g.ID, &g.AttemptID, &g.Gate, &g.Passed, &g.Flagged, &g.ArtifactHash, &g.Mode,
			&g.Detail, &g.ArtifactKey, &g.RanAt, &durationMS); err != nil {
			return nil, err
		}
		g.RanAt = g.RanAt.UTC()
		g.Duration = time.Duration(durationMS) * time.Millisecond
		out = append(out, g)
	}
	return out, rows.Err()
}

// checkGateBatch refuses a batch the table would reject, or that does not
// describe one artifact, before any SQL runs: an error inside a caller's
// transaction would abort it.
func checkGateBatch(results []contracts.GateResult) error {
	if len(results) == 0 {
		return errors.New("no results")
	}
	first := results[0]
	seen := make(map[contracts.GateName]bool, len(results))
	for _, res := range results {
		if res.AttemptID != first.AttemptID || res.ArtifactHash != first.ArtifactHash {
			return errors.New("results for more than one attempt or artifact in one batch")
		}
		if seen[res.Gate] {
			return fmt.Errorf("gate %q appears twice", res.Gate)
		}
		seen[res.Gate] = true
		if err := checkGateResult(res); err != nil {
			return fmt.Errorf("gate %q: %w", res.Gate, err)
		}
	}
	return nil
}

func checkGateResult(res contracts.GateResult) error {
	if err := checkUUID("attempt ID", res.AttemptID); err != nil {
		return err
	}
	switch {
	case res.Gate == "":
		return errors.New("empty gate name")
	case !artifactHashRE.MatchString(res.ArtifactHash):
		return fmt.Errorf("artifact hash %q is not lowercase hex SHA-256", res.ArtifactHash)
	case res.Mode != contracts.GatingCleanRoom && res.Mode != contracts.GatingResetInPlace:
		return fmt.Errorf("unknown gating mode %q", res.Mode)
	case res.Duration < 0:
		return fmt.Errorf("negative duration %s", res.Duration)
	case res.RanAt.IsZero():
		return errors.New("no ran-at time: the gate must stamp it")
	}
	for _, s := range []string{string(res.Gate), res.Detail, res.ArtifactKey} {
		if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			return errors.New("text that Postgres cannot store: invalid UTF-8 or NUL")
		}
	}
	return nil
}
