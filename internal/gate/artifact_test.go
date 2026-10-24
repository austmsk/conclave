package gate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/blob"
)

const (
	baseA = "1111111111111111111111111111111111111111"
	baseB = "2222222222222222222222222222222222222222"
)

var sampleDiff = []byte("diff --git a/README.md b/README.md\n" +
	"index 2c76df8..12799cc 100644\n" +
	"--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-# Election Tally\n+good\n")

// testArtifact builds the Artifact Freeze would return for base and diff.
func testArtifact(base string, diff []byte) Artifact {
	hash := HashArtifact(base, diff)
	return Artifact{
		RepoID: testRepoID,
		Base:   base,
		Hash:   hash,
		Key:    testRepoID + "/" + hash,
		Size:   int64(len(Envelope(base, diff))),
	}
}

// freezeFake freezes a fake sandbox scripted with diffs.
func freezeFake(t *testing.T, blobs blob.Store, base string, diffs ...[]byte) (Artifact, error) {
	t.Helper()
	sb := newFakeSandbox("agent")
	sb.diffs = diffs
	return Freeze(context.Background(), sb, blobs, testRepoID, base)
}

// TestHashBindsBase: the same diff on another base is another artifact,
// and each key loads back with its own base.
func TestHashBindsBase(t *testing.T) {
	blobs := newTestBlobs(t)
	a, err := freezeFake(t, blobs, baseA, sampleDiff)
	if err != nil {
		t.Fatal(err)
	}
	b, err := freezeFake(t, blobs, baseB, sampleDiff)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash == b.Hash || a.Key == b.Key {
		t.Fatalf("one diff on two bases hashed alike: %s", a.Hash)
	}
	for _, want := range []Artifact{a, b} {
		got, diff, err := Load(context.Background(), blobs, want.Key)
		if err != nil {
			t.Fatalf("Load(%s): %v", want.Key, err)
		}
		if got != want || !bytes.Equal(diff, sampleDiff) {
			t.Errorf("Load(%s) = %+v, %q; want %+v, the frozen diff", want.Key, got, diff, want)
		}
	}
}

// TestFreezeTwiceSameHash: freezing the same workspace twice (a retried
// activity) yields the same artifact and key (RL-2).
func TestFreezeTwiceSameHash(t *testing.T) {
	blobs := newTestBlobs(t)
	first, err := freezeFake(t, blobs, baseA, sampleDiff)
	if err != nil {
		t.Fatal(err)
	}
	second, err := freezeFake(t, blobs, baseA, sampleDiff)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("second freeze = %+v, first %+v", second, first)
	}
	if first != testArtifact(baseA, sampleDiff) {
		t.Errorf("artifact = %+v, want %+v", first, testArtifact(baseA, sampleDiff))
	}
	env, err := blobs.Get(context.Background(), first.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(env, Envelope(baseA, sampleDiff)) {
		t.Errorf("stored envelope = %q", env)
	}
}

// TestFreezeRefusesOversizedDiff: a diff past MaxArtifactBytes is the
// attempt's own failure and nothing is stored; one at the cap is frozen.
func TestFreezeRefusesOversizedDiff(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantErr error
	}{
		{name: "at cap", size: MaxArtifactBytes},
		{name: "one past cap", size: MaxArtifactBytes + 1, wantErr: ErrArtifactTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			blobs, err := blob.NewDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blobs.Close() }()
			_, err = freezeFake(t, blobs, baseA, bytes.Repeat([]byte("+"), tt.size))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Freeze error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil {
				return
			}
			if errors.Is(err, ErrEnvironment) {
				t.Errorf("oversized diff classified as environment: %v", err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("oversized diff stored something: %v", entries)
			}
		})
	}
}

// TestFreezeUnstableDiff: two reads that disagree mean something still
// changes the workspace, and nothing is frozen.
func TestFreezeUnstableDiff(t *testing.T) {
	changed := append(append([]byte(nil), sampleDiff...), "+more\n"...)
	_, err := freezeFake(t, newTestBlobs(t), baseA, sampleDiff, changed)
	if !errors.Is(err, ErrUnstableDiff) {
		t.Fatalf("Freeze error = %v, want ErrUnstableDiff", err)
	}
}

// TestFreezeRefusals covers the other ways Freeze refuses: a base that is
// not a full commit ID, and a sandbox whose Diff fails (the environment's
// fault).
func TestFreezeRefusals(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		diffErr error
		wantEnv bool
	}{
		{name: "abbreviated base", base: "1111111"},
		{name: "ref name base", base: "main"},
		{name: "uppercase base", base: strings.ToUpper("abcdef" + baseA[6:])},
		{name: "diff fails", base: baseA, diffErr: errors.New("exec: sandbox killed"), wantEnv: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := newFakeSandbox("agent")
			sb.diffErr = tt.diffErr
			if tt.diffErr == nil {
				sb.diffs = [][]byte{sampleDiff}
			}
			_, err := Freeze(context.Background(), sb, newTestBlobs(t), testRepoID, tt.base)
			if err == nil {
				t.Fatal("Freeze succeeded")
			}
			if errors.Is(err, ErrEnvironment) != tt.wantEnv {
				t.Errorf("Freeze error = %v, environment = %t, want %t", err, !tt.wantEnv, tt.wantEnv)
			}
		})
	}
}

// TestArtifactBlobTamperDetected: one byte changed in the stored envelope
// on disk makes Load fail with blob.ErrCorrupt, never return the bytes.
func TestArtifactBlobTamperDetected(t *testing.T) {
	dir := t.TempDir()
	blobs, err := blob.NewDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blobs.Close() }()
	art, err := freezeFake(t, blobs, baseA, sampleDiff)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, testRepoID, art.Hash)
	env, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env[len(env)-2] ^= 0x01
	if err := os.WriteFile(path, env, 0o600); err != nil {
		t.Fatal(err)
	}
	_, diff, err := Load(context.Background(), blobs, art.Key)
	if !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("Load of tampered blob: err = %v, want blob.ErrCorrupt", err)
	}
	if diff != nil {
		t.Errorf("Load returned bytes of a tampered blob")
	}
}
