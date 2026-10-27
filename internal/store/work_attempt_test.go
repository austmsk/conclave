package store_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// stepCase is one call to an attempt update, run in order against one
// attempt, in the caller's transaction and without savepoints.
type stepCase struct {
	name    string
	call    func() error
	wantErr error
}

func runSteps(t *testing.T, cases []stepCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, tc.call(), tc.wantErr)
		})
	}
}

func attemptText(t *testing.T, ctx context.Context, tx pgx.Tx, column, id string) string {
	t.Helper()
	var v string
	if err := tx.QueryRow(ctx, `SELECT coalesce(`+column+`, '') FROM attempts WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("reading %s of attempt %s: %v", column, id, err)
	}
	return v
}

func TestSetAttemptSandbox(t *testing.T) {
	ctx, tx := testTx(t)
	c := createChain(t, ctx, tx, "austmsk/sandbox", 1)
	set := func(attempt, sandbox string) func() error {
		return func() error { return store.SetAttemptSandbox(ctx, tx, attempt, sandbox) }
	}
	runSteps(t, []stepCase{
		{"records", set(c.attempt.ID, "conclave-abc123"), nil},
		{"same again", set(c.attempt.ID, "conclave-abc123"), nil},
		{"another sandbox", set(c.attempt.ID, "conclave-def456"), store.ErrSandboxMismatch},
		{"malformed attempt ID", set("attempt-1", "conclave-abc123"), store.ErrInvalidID},
		{"unknown attempt", set(unknownID, "conclave-abc123"), store.ErrNotFound},
		{"empty sandbox ID", set(c.attempt.ID, ""), errAny},
		{"NUL in sandbox ID", set(c.attempt.ID, "a\x00b"), errAny},
	})
	if got := attemptText(t, ctx, tx, "sandbox_id", c.attempt.ID); got != "conclave-abc123" {
		t.Errorf("sandbox_id = %q, want the first one recorded", got)
	}
}

func TestMarkSandboxReleased(t *testing.T) {
	ctx, tx := testTx(t)
	c := createChain(t, ctx, tx, "austmsk/release", 1)
	mark := func(attempt string) func() error {
		return func() error { return store.MarkSandboxReleased(ctx, tx, attempt) }
	}
	// now() is fixed for a transaction, so a second stamp would be
	// indistinguishable from the first. Moving the first back an hour
	// makes an overwrite visible.
	const backdate = `UPDATE attempts SET sandbox_released_at = '2026-10-01T00:00:00Z' WHERE id = $1`
	runSteps(t, []stepCase{
		{"stamps", mark(c.attempt.ID), nil},
		{"backdate the stamp", func() error { _, err := tx.Exec(ctx, backdate, c.attempt.ID); return err }, nil},
		{"again is a no-op", mark(c.attempt.ID), nil},
		{"malformed attempt ID", mark("attempt-1"), store.ErrInvalidID},
		{"unknown attempt", mark(unknownID), store.ErrNotFound},
	})
	var released time.Time
	if err := tx.QueryRow(ctx, `SELECT sandbox_released_at FROM attempts WHERE id = $1`, c.attempt.ID).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !released.Equal(want) {
		t.Errorf("sandbox_released_at = %s, want the first stamp %s left alone", released, want)
	}
}

// TestSetArtifactHash: the hash is set once, the same value again is a
// retry, and a different value is ErrArtifactMismatch from the store;
// past the store, a direct UPDATE is still refused by 00007's trigger.
func TestSetArtifactHash(t *testing.T) {
	ctx, tx := testTx(t)
	c := createChain(t, ctx, tx, "austmsk/artifact", 1)
	set := func(attempt, hash string) func() error {
		return func() error { return store.SetArtifactHash(ctx, tx, attempt, hash) }
	}
	runSteps(t, []stepCase{
		{"records", set(c.attempt.ID, gateHash), nil},
		{"same again", set(c.attempt.ID, gateHash), nil},
		{"another hash", set(c.attempt.ID, otherHash), store.ErrArtifactMismatch},
		{"uppercase hash", set(c.attempt.ID, "A1"+gateHash[2:]), errAny},
		{"short hash", set(c.attempt.ID, gateHash[:63]), errAny},
		{"malformed attempt ID", set("attempt-1", gateHash), store.ErrInvalidID},
		{"unknown attempt", set(unknownID, gateHash), store.ErrNotFound},
	})
	if got := attemptText(t, ctx, tx, "artifact_hash", c.attempt.ID); got != gateHash {
		t.Errorf("artifact_hash = %q, want the first one recorded", got)
	}
	sp := savepoint(t, ctx, tx)
	_, err := sp.Exec(ctx, `UPDATE attempts SET artifact_hash = $2 WHERE id = $1`, c.attempt.ID, otherHash)
	if got := sqlState(err); got != integrityViolation {
		t.Errorf("direct UPDATE of the hash: got %v (SQLSTATE %q), want %s", err, got, integrityViolation)
	}
}

func finishParams(attempt string) store.FinishParams {
	return store.FinishParams{
		AttemptID: attempt, Revision: 0,
		To: contracts.AttemptFailed, Reason: contracts.ReasonGateFailed,
		Actor: store.ActorWorkflow("feature-12"),
		Usage: contracts.Usage{InputTokens: 1200, OutputTokens: 300, CostUSD: 0.0125, Steps: 7},
		Model: "claude-fable-5-2", Provider: "anthropic",
		Report: &contracts.CompletionReport{Summary: "Added export", FilesChanged: []string{"src/Export.java"},
			TestsAdded: []string{}, Decisions: []contracts.Decision{}, Uncertainties: []string{}},
	}
}

// TestFinishAttempt finishes an attempt, then retries the way a
// retried activity would: identically, and with different numbers. Both
// retries are ErrAlreadyApplied and change nothing, so there is one audit
// row and one set of usage values. Inputs the table or Transition would
// refuse are refused before any SQL runs, so the transaction survives
// them.
func TestFinishAttempt(t *testing.T) {
	ctx, tx := testTx(t)
	c := createChain(t, ctx, tx, "austmsk/finish", 1)
	finish := func(apply func(*store.FinishParams)) func() error {
		return func() error {
			p := finishParams(c.attempt.ID)
			apply(&p)
			return store.FinishAttempt(ctx, tx, p)
		}
	}
	runSteps(t, []stepCase{
		{"passed with a reason", finish(func(p *store.FinishParams) { p.To = contracts.AttemptPassed }), errAny},
		{"failed without a reason", finish(func(p *store.FinishParams) { p.Reason = "" }), errAny},
		{"back to running", finish(func(p *store.FinishParams) { p.To = contracts.AttemptRunning }), errAny},
		{"no actor", finish(func(p *store.FinishParams) { p.Actor = "" }), errAny},
		{"negative tokens", finish(func(p *store.FinishParams) { p.Usage.InputTokens = -1 }), errAny},
		{"cost past the column", finish(func(p *store.FinishParams) { p.Usage.CostUSD = 1e7 }), errAny},
		{"malformed attempt ID", finish(func(p *store.FinishParams) { p.AttemptID = "attempt-1" }), store.ErrInvalidID},
		{"unknown attempt", finish(func(p *store.FinishParams) { p.AttemptID = unknownID }), store.ErrNotFound},
		{"finishes", finish(func(*store.FinishParams) {}), nil},
		{"identical retry", finish(func(*store.FinishParams) {}), store.ErrAlreadyApplied},
		{"retry with other usage", finish(func(p *store.FinishParams) {
			p.Usage = contracts.Usage{InputTokens: 9, OutputTokens: 9, CostUSD: 9, Steps: 9}
			p.Report = nil
		}), store.ErrAlreadyApplied},
	})
	// A stale call to another terminal state lost the race and must not
	// be mistaken for its own retry.
	stale := finishParams(c.attempt.ID)
	stale.To, stale.Reason = contracts.AttemptErrored, contracts.ReasonCancelled
	var wrongState *store.WrongStateError
	if err := store.FinishAttempt(ctx, tx, stale); !errors.As(err, &wrongState) {
		t.Errorf("stale finish: got %v, want a WrongStateError", err)
	}

	want := []audit{{kind: "attempt", from: "running", to: "failed", actor: "workflow:feature-12",
		dbUser: sessionUser(t, ctx, tx), reason: "gate_failed", revision: 1}}
	if got := audits(t, ctx, tx, c.attempt.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("audit rows = %+v, want %+v", got, want)
	}
	got := readAttempt(t, ctx, tx, c.attempt)
	first := finishParams(c.attempt.ID)
	if got.Usage != first.Usage || got.Provenance.Model != first.Model || !reflect.DeepEqual(got.Report, first.Report) {
		t.Errorf("stored usage %+v, model %q, report %+v: want the first call's", got.Usage, got.Provenance.Model, got.Report)
	}
	if got.State != contracts.AttemptFailed || got.Reason != contracts.ReasonGateFailed || got.FinishedAt.IsZero() {
		t.Errorf("stored state %q, reason %q, finished %s", got.State, got.Reason, got.FinishedAt)
	}
}

// readAttempt reads an attempt back through CreateAttempt's idempotent
// path, which returns the stored row.
func readAttempt(t *testing.T, ctx context.Context, tx pgx.Tx, a contracts.Attempt) contracts.Attempt {
	t.Helper()
	got, created, err := store.CreateAttempt(ctx, tx, store.AttemptParams{TaskID: a.TaskID, Ordinal: a.Ordinal, Provenance: testProvenance()})
	if err != nil || created {
		t.Fatalf("reading attempt %s: created %v, err %v", a.ID, created, err)
	}
	return got
}

// TestUnreleasedAttempts sets up one attempt per condition the reaper's
// query distinguishes and checks that only the old, recorded, unreleased
// sandbox is listed.
func TestUnreleasedAttempts(t *testing.T) {
	ctx, tx := testTx(t)
	c := createChain(t, ctx, tx, "austmsk/unreleased", 1)
	cases := []struct {
		name       string
		sandbox    string
		released   bool
		age        string
		wantListed bool
	}{
		{"old and unreleased", "conclave-old", false, "2 hours", true},
		{"old and released", "conclave-released", true, "2 hours", false},
		{"too recent", "conclave-recent", false, "1 minute", false},
		{"no sandbox recorded", "", false, "2 hours", false},
	}
	byID := map[string]string{}
	for i, tc := range cases {
		a, _, err := store.CreateAttempt(ctx, tx, store.AttemptParams{TaskID: c.task.ID, Ordinal: i + 2, Provenance: testProvenance()})
		if err != nil {
			t.Fatal(err)
		}
		byID[a.ID] = tc.name
		setUpUnreleased(t, ctx, tx, a.ID, tc.sandbox, tc.released, tc.age)
	}

	got, err := store.UnreleasedAttempts(ctx, tx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, u := range got {
		name, ours := byID[u.Attempt.ID]
		if !ours {
			continue // a row another test left committed
		}
		listed = append(listed, name)
		if u.SandboxID != "conclave-old" || u.Attempt.State != contracts.AttemptRunning || u.Attempt.TaskID != c.task.ID {
			t.Errorf("listed %+v", u)
		}
	}
	for _, tc := range cases {
		if slices.Contains(listed, tc.name) != tc.wantListed {
			t.Errorf("%s: listed = %v, want %v", tc.name, !tc.wantListed, tc.wantListed)
		}
	}
	if _, err := store.UnreleasedAttempts(ctx, tx, -time.Second); err == nil {
		t.Error("negative age accepted")
	}
}

func setUpUnreleased(t *testing.T, ctx context.Context, tx pgx.Tx, id, sandbox string, released bool, age string) {
	t.Helper()
	if sandbox != "" {
		if err := store.SetAttemptSandbox(ctx, tx, id, sandbox); err != nil {
			t.Fatal(err)
		}
	}
	if released {
		if err := store.MarkSandboxReleased(ctx, tx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE attempts SET started_at = now() - $2::interval WHERE id = $1`, id, age); err != nil {
		t.Fatalf("backdating attempt: %v", err)
	}
}
