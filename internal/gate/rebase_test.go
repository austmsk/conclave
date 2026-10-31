package gate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
)

// rebasedDiff is what the fake sandbox reports after the three-way apply:
// the same change, as diffed against the new base.
var rebasedDiff = []byte("diff --git a/README.md b/README.md\n" +
	"index 3d8a1f0..12799cc 100644\n" +
	"--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-# Election Tally\n+good\n")

// applyExit makes the fake sandbox answer git apply with exit code; every
// other command succeeds.
func applyExit(code int) func(contracts.ExecRequest) (contracts.ExecResult, error) {
	return func(req contracts.ExecRequest) (contracts.ExecResult, error) {
		if strings.Contains(strings.Join(req.Command, " "), "git apply") {
			return contracts.ExecResult{ExitCode: code, Stderr: []byte("error: patch failed: secret/name.go:1")}, nil
		}
		return contracts.ExecResult{}, nil
	}
}

// TestRebase: a diff that applies is uploaded, moved out of the
// workspace, applied three-way with isolated git, and frozen against the
// new base.
func TestRebase(t *testing.T) {
	original := testArtifact(baseA, sampleDiff)
	sb := newFakeSandbox("rebase")
	sb.exec = applyExit(0)
	sb.diffs = [][]byte{rebasedDiff}
	blobs := newTestBlobs(t)
	got, err := Rebase(t.Context(), sb, blobs, original, sampleDiff, baseB)
	if err != nil {
		t.Fatal(err)
	}
	if want := testArtifact(baseB, rebasedDiff); got != want {
		t.Fatalf("Rebase = %+v, want %+v", got, want)
	}
	if _, diff, err := Load(t.Context(), blobs, got.Key); err != nil || string(diff) != string(rebasedDiff) {
		t.Fatalf("stored rebased artifact: %q, %v", diff, err)
	}
	if string(sb.writes[rebaseDiffName]) != string(sampleDiff) {
		t.Errorf("uploaded %q, want the recorded diff", sb.writes[rebaseDiffName])
	}
	apply := sb.ran("git apply --3way --binary")
	if apply < 0 || sb.ran("mv -- /workspace/"+rebaseDiffName) > apply || sb.ran("GIT_CONFIG_GLOBAL=/dev/null") != apply ||
		!strings.HasSuffix(sb.commands()[apply], " "+rebaseDiffPath) {
		t.Errorf("commands %q: want the diff moved out, then applied three-way with isolated git", sb.commands())
	}
}

// TestRebaseApplyFails: a diff git refuses is a conflict, and a sandbox
// without git is the environment's failure. Neither quotes git's output
// or freezes anything.
func TestRebaseApplyFails(t *testing.T) {
	cases := []struct {
		name    string
		exit    int
		wantErr error
	}{
		{"conflict", 1, ErrRebaseConflict},
		{"git missing", exitNotFound, ErrEnvironment},
		{"env not executable", exitNotExecutable, ErrEnvironment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := newFakeSandbox("rebase")
			sb.exec = applyExit(tc.exit)
			sb.diffs = [][]byte{rebasedDiff}
			_, err := Rebase(t.Context(), sb, newTestBlobs(t), testArtifact(baseA, sampleDiff), sampleDiff, baseB)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Rebase error = %v, want %v", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "secret/name.go") {
				t.Errorf("error %q quotes git's output", err)
			}
			if sb.diffCalls != 0 {
				t.Errorf("froze after a failed apply")
			}
		})
	}
}

func TestRebaseSameBaseIsUnchanged(t *testing.T) {
	original := testArtifact(baseA, sampleDiff)
	sb := newFakeSandbox("untouched")
	got, err := Rebase(t.Context(), sb, newTestBlobs(t), original, sampleDiff, baseA)
	if err != nil || got != original {
		t.Fatalf("Rebase = %+v, %v; want the original back", got, err)
	}
	if len(sb.commands()) != 0 || len(sb.writeOrder) != 0 || sb.diffCalls != 0 {
		t.Errorf("sandbox was touched: %q", sb.commands())
	}
}

// TestRebaseRefusesCallerErrors: a diff that is not the artifact's, or a
// base that is not a full commit ID, is refused before the sandbox runs
// anything.
func TestRebaseRefusesCallerErrors(t *testing.T) {
	original := testArtifact(baseA, sampleDiff)
	cases := []struct {
		name string
		diff []byte
		base string
	}{
		{"another diff", rebasedDiff, baseB},
		{"short base", sampleDiff, "2222222"},
		{"ref name", sampleDiff, "main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := newFakeSandbox("refused")
			if _, err := Rebase(t.Context(), sb, newTestBlobs(t), original, tc.diff, tc.base); err == nil {
				t.Fatal("Rebase accepted a bad input")
			}
			if len(sb.commands()) != 0 || len(sb.writeOrder) != 0 {
				t.Errorf("sandbox was touched: %q", sb.commands())
			}
		})
	}
}

func TestRebaseCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sb := newFakeSandbox("cancelled")
	_, err := Rebase(ctx, sb, newTestBlobs(t), testArtifact(baseA, sampleDiff), sampleDiff, baseB)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrEnvironment) || errors.Is(err, ErrRebaseConflict) {
		t.Fatalf("Rebase error = %v, want the context's error", err)
	}
}
