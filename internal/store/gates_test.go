package store_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/store"
)

var (
	_ gate.Results          = (*store.GateResults)(nil)
	_ gate.RejectionCounter = (*store.ToolCalls)(nil)
)

var (
	gateHash  = strings.Repeat("a1", 32)
	otherHash = strings.Repeat("b2", 32)
)

// gateBatch is one artifact's results: diff_size passed, secrets failed.
func gateBatch(attempt, hash string) []contracts.GateResult {
	at := time.Date(2026, 10, 23, 12, 0, 0, 123456789, time.UTC)
	return []contracts.GateResult{
		{AttemptID: attempt, Gate: contracts.GateSecrets, Passed: false, ArtifactHash: hash,
			Mode: contracts.GatingCleanRoom, Detail: "1 secret finding(s)", RanAt: at, Duration: 1500 * time.Millisecond},
		{AttemptID: attempt, Gate: contracts.GateDiffSize, Passed: true, Flagged: true, ArtifactHash: hash,
			Mode: contracts.GatingCleanRoom, ArtifactKey: "repo/" + hash, RanAt: at, Duration: 2 * time.Millisecond},
	}
}

// asStored is what the table holds for r: millisecond duration, microsecond
// time, the database's ID.
func asStored(r contracts.GateResult, id string) contracts.GateResult {
	r.ID = id
	r.RanAt = r.RanAt.Truncate(time.Microsecond)
	r.Duration = r.Duration.Truncate(time.Millisecond)
	return r
}

func countGateRows(t *testing.T, ctx context.Context, tx pgx.Tx, attempt string) int {
	t.Helper()
	return countRows(t, ctx, tx, `SELECT count(*) FROM gate_results WHERE attempt_id = $1`, attempt)
}

// TestGateResults_RecordIdempotentFirstWriterWins replays a batch the way
// a retried activity does, then records a different verdict for the same
// artifact. Every write succeeds as the application role, nothing is
// added after the first, and every caller gets the first rows back (RL-2).
func TestGateResults_RecordIdempotentFirstWriterWins(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	gr := store.NewGateResults(tx)

	first, err := gr.Record(ctx, gateBatch(s.attempt, gateHash))
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	batch := gateBatch(s.attempt, gateHash)
	want := []contracts.GateResult{asStored(batch[1], first[0].ID), asStored(batch[0], first[1].ID)}
	if !slices.EqualFunc(first, want, sameResult) {
		t.Fatalf("stored rows:\n%+v\nwant, ordered by gate:\n%+v", first, want)
	}

	flipped := gateBatch(s.attempt, gateHash)
	flipped[0].Passed, flipped[0].Detail = true, ""
	for name, b := range map[string][]contracts.GateResult{"replay": gateBatch(s.attempt, gateHash), "flipped": flipped} {
		got, err := gr.Record(ctx, b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.EqualFunc(got, want, sameResult) {
			t.Errorf("%s returned %+v, want the first rows", name, got)
		}
	}
	if n := countGateRows(t, ctx, tx, s.attempt); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
	read, err := gr.ForArtifact(ctx, s.attempt, gateHash)
	if err != nil || !slices.EqualFunc(read, want, sameResult) {
		t.Errorf("ForArtifact = %+v, %v; want the first rows", read, err)
	}
	if other, err := gr.ForArtifact(ctx, s.attempt, otherHash); err != nil || len(other) != 0 {
		t.Errorf("ForArtifact(other hash) = %+v, %v; want none", other, err)
	}
}

func sameResult(a, b contracts.GateResult) bool {
	return a.ID == b.ID && a.RanAt.Equal(b.RanAt) && withoutTime(a) == withoutTime(b)
}

func withoutTime(r contracts.GateResult) contracts.GateResult {
	r.RanAt = time.Time{}
	return r
}

// TestGateResultsRecordRefusesBadInput: a batch the table would reject, or
// that is not one artifact's, is refused before any SQL, so the caller's
// transaction survives and nothing is written.
func TestGateResultsRecordRefusesBadInput(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	gr := store.NewGateResults(tx)
	cases := []struct {
		name   string
		change func([]contracts.GateResult) []contracts.GateResult
	}{
		{"empty batch", func([]contracts.GateResult) []contracts.GateResult { return nil }},
		{"attempt ID not a UUID", func(b []contracts.GateResult) []contracts.GateResult {
			b[0].AttemptID, b[1].AttemptID = "a1", "a1"
			return b
		}},
		{"uppercase hash", func(b []contracts.GateResult) []contracts.GateResult {
			h := strings.ToUpper(gateHash)
			b[0].ArtifactHash, b[1].ArtifactHash = h, h
			return b
		}},
		{"two artifacts", func(b []contracts.GateResult) []contracts.GateResult { b[1].ArtifactHash = otherHash; return b }},
		{"same gate twice", func(b []contracts.GateResult) []contracts.GateResult { b[1].Gate = b[0].Gate; return b }},
		{"empty gate", func(b []contracts.GateResult) []contracts.GateResult { b[0].Gate = ""; return b }},
		{"unknown mode", func(b []contracts.GateResult) []contracts.GateResult { b[0].Mode = "dirty"; return b }},
		{"negative duration", func(b []contracts.GateResult) []contracts.GateResult { b[0].Duration = -time.Second; return b }},
		{"no ran-at", func(b []contracts.GateResult) []contracts.GateResult { b[0].RanAt = time.Time{}; return b }},
		{"NUL in detail", func(b []contracts.GateResult) []contracts.GateResult { b[0].Detail = "a\x00b"; return b }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := gr.Record(ctx, tc.change(gateBatch(s.attempt, gateHash))); err == nil {
				t.Fatalf("Record accepted it: %+v", got)
			}
		})
	}
	if n := countGateRows(t, ctx, tx, s.attempt); n != 0 {
		t.Errorf("%d rows written by refused batches", n)
	}
}

// TestGateResultsUpdateDenied attempts the rewrite the push path must not
// trust: flipping a recorded result as the application role. Migration
// 00007 withdrew UPDATE, so the database refuses it.
func TestGateResultsUpdateDenied(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	if _, err := store.NewGateResults(tx).Record(ctx, gateBatch(s.attempt, gateHash)); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, sql string }{
		{"flip passed", `UPDATE gate_results SET passed = true`},
		{"repoint hash", `UPDATE gate_results SET artifact_hash = '` + otherHash + `'`},
		{"delete", `DELETE FROM gate_results`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := savepoint(t, ctx, tx)
			if _, err := sp.Exec(ctx, tc.sql); sqlState(err) != permissionDenied {
				t.Fatalf("got %v (SQLSTATE %q), want permission denied", err, sqlState(err))
			}
		})
	}
}

// TestGateResultsHashCheck inserts as the owner, past every Go check, and
// the CHECK from 00007 still refuses a hash that is not lowercase hex
// SHA-256.
func TestGateResultsHashCheck(t *testing.T) {
	ctx := requireDB(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	s := seed(t, ctx, tx)
	cases := []struct{ name, hash, want string }{
		{"lowercase sha-256", gateHash, ""},
		{"uppercase", strings.ToUpper(otherHash), checkViolation},
		{"short", "abc123", checkViolation},
		{"not hex", strings.Repeat("g", 64), checkViolation},
		{"trailing newline", otherHash + "\n", checkViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := savepoint(t, ctx, tx)
			_, err := sp.Exec(ctx, `INSERT INTO gate_results (attempt_id, gate, passed, artifact_hash, mode)
				VALUES ($1, 'secrets', true, $2, 'clean_room')`, s.attempt, tc.hash)
			if got := sqlState(err); got != tc.want {
				t.Fatalf("got %v (SQLSTATE %q), want SQLSTATE %q", err, got, tc.want)
			}
		})
	}
}

// TestArtifactHashWriteOnce: once an attempt records the hash of its
// frozen artifact, the application role cannot point it at another.
// Setting it from NULL is the freeze; writing the same value again is a
// retried freeze and allowed.
func TestArtifactHashWriteOnce(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	set := func(db pgx.Tx, v any) error {
		_, err := db.Exec(ctx, `UPDATE attempts SET artifact_hash = $2 WHERE id = $1`, s.attempt, v)
		return err
	}
	if err := set(tx, gateHash); err != nil {
		t.Fatalf("NULL to a hash: %v", err)
	}
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"same hash again", gateHash, ""},
		{"another hash", otherHash, integrityViolation},
		{"back to NULL", nil, integrityViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := savepoint(t, ctx, tx)
			if got := sqlState(set(sp, tc.value)); got != tc.want {
				t.Fatalf("got SQLSTATE %q, want %q", got, tc.want)
			}
		})
	}
	other := `UPDATE attempts SET model = 'claude-fable-5-2' WHERE id = $1`
	if _, err := tx.Exec(ctx, other, s.attempt); err != nil {
		t.Errorf("updating another column of an attempt with a hash: %v", err)
	}
}

// integrityViolation is the SQLSTATE the write-once trigger raises.
const integrityViolation = "23000"

// TestGateIntegrityMigrationDown rolls back 00007 alone and re-applies it:
// the down migration restores UPDATE and removes the CHECK and the
// trigger, and the up migration puts them back.
func TestGateIntegrityMigrationDown(t *testing.T) {
	ctx := requireDB(t)
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("migrations"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Migrate(context.Background(), sqlDB); err != nil {
			t.Errorf("re-migrating after the test: %v", err)
		}
	})
	if _, err := p.DownTo(ctx, 6); err != nil {
		t.Fatalf("down to 6: %v", err)
	}
	checkGateIntegrity(t, ctx, false)
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	checkGateIntegrity(t, ctx, true)
}

func checkGateIntegrity(t *testing.T, ctx context.Context, applied bool) {
	t.Helper()
	checks := []struct {
		name    string
		query   string
		present bool
	}{
		{"UPDATE on gate_results", `SELECT has_table_privilege('conclave_app', 'gate_results', 'UPDATE')`, !applied},
		{"hash CHECK", `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'gate_results_artifact_hash_sha256')`, applied},
		{"write-once trigger", `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'attempts_artifact_hash_write_once')`, applied},
		{"trigger function", `SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'attempts_artifact_hash_write_once')`, applied},
	}
	for _, c := range checks {
		if got := exists(t, ctx, c.query); got != c.present {
			t.Errorf("00007 applied=%v: %s present = %v, want %v", applied, c.name, got, c.present)
		}
	}
}
