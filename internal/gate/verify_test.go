package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
)

const (
	w3RepoID    = "5b8c1f0e-3d2a-4c6b-9e7f-0a1b2c3d4e5f"
	w3AttemptID = "9d4e2a1b-7c3f-4e8d-a6b5-1c2d3e4f5a6b"
	w3Base      = "0123456789abcdef0123456789abcdef01234567"
	w3Diff      = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n"
)

// w3Blobs stores one artifact in a fresh blob directory and returns the
// store, the directory, and the artifact's key and hash.
func w3Blobs(t *testing.T) (store blob.Store, dir, key, hash string) {
	t.Helper()
	dir = t.TempDir()
	d, err := blob.NewDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	key, err = d.Put(context.Background(), w3RepoID, Envelope(w3Base, []byte(w3Diff)))
	if err != nil {
		t.Fatal(err)
	}
	return d, dir, key, HashArtifact(w3Base, []byte(w3Diff))
}

// passingRows is a passing result for every required gate.
func passingRows(attemptID, hash string) []contracts.GateResult {
	rows := make([]contracts.GateResult, 0, len(RequiredGates))
	for _, g := range RequiredGates {
		rows = append(rows, contracts.GateResult{
			AttemptID: attemptID, Gate: g, Passed: true, ArtifactHash: hash,
			Mode: contracts.GatingCleanRoom, RanAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC),
		})
	}
	return rows
}

func TestVerify(t *testing.T) {
	other := strings.Repeat("e", 64)
	cases := []struct {
		name    string
		rows    func(hash string) []contracts.GateResult
		forErr  error
		wantErr error
		wantMsg string
	}{
		{name: "every gate passed", rows: func(h string) []contracts.GateResult { return passingRows(w3AttemptID, h) }},
		{name: "flagged gates still pass", rows: func(h string) []contracts.GateResult {
			rows := passingRows(w3AttemptID, h)
			rows[3].Flagged = true
			return rows
		}},
		{name: "a gate missing", rows: func(h string) []contracts.GateResult {
			return passingRows(w3AttemptID, h)[:len(RequiredGates)-1]
		}, wantErr: ErrNotVerified, wantMsg: "no result for tests"},
		{name: "a gate failed", rows: func(h string) []contracts.GateResult {
			rows := passingRows(w3AttemptID, h)
			rows[7].Passed = false
			return rows
		}, wantErr: ErrNotVerified, wantMsg: "failed lint"},
		{name: "rows for another artifact", rows: func(string) []contracts.GateResult {
			return passingRows(w3AttemptID, other)
		}, wantErr: ErrNotVerified, wantMsg: "no result for diff_size"},
		{name: "no rows", rows: func(string) []contracts.GateResult { return nil }, wantErr: ErrNotVerified},
		{name: "store unreachable", rows: func(string) []contracts.GateResult { return nil },
			forErr: errors.New("connection refused"), wantErr: ErrEnvironment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blobs, _, key, hash := w3Blobs(t)
			results := newFakeResults()
			results.put(tc.rows(hash)...)
			results.forErr = tc.forErr
			art, err := Verify(context.Background(), blobs, results, w3AttemptID, key)
			if tc.wantErr == nil {
				if err != nil || art.Hash != hash || art.Key != key || art.Base != w3Base {
					t.Fatalf("Verify = %+v, %v; want the artifact %s", art, err, hash)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("err = %v, want %v mentioning %q", err, tc.wantErr, tc.wantMsg)
			}
			if art != (Artifact{}) {
				t.Errorf("returned artifact %+v with an error", art)
			}
		})
	}
}

// TestVerifyRowsForAnotherAttempt: passing rows recorded under another
// attempt, for the same artifact, do not verify this attempt.
func TestVerifyRowsForAnotherAttempt(t *testing.T) {
	blobs, _, key, hash := w3Blobs(t)
	results := newFakeResults()
	results.put(passingRows("0e1d2c3b-4a59-4687-b7a6-958473625140", hash)...)
	if _, err := Verify(context.Background(), blobs, results, w3AttemptID, key); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("err = %v, want ErrNotVerified", err)
	}
}

// TestVerifyTamperedBlob: one byte of the stored artifact changes on disk
// after gating. Verify re-hashes it and refuses, though every result row
// for the original hash passed.
func TestVerifyTamperedBlob(t *testing.T) {
	blobs, dir, key, hash := w3Blobs(t)
	results := newFakeResults()
	results.put(passingRows(w3AttemptID, hash)...)

	path := filepath.Join(dir, filepath.FromSlash(key))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-2] ^= 1
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	art, err := Verify(context.Background(), blobs, results, w3AttemptID, key)
	if !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("Verify = %+v, %v; want blob.ErrCorrupt", art, err)
	}
}
