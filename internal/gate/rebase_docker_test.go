package gate

import (
	"errors"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

// rebaseAgentChange edits the line the conflicting base move also edits,
// and adds a file the clean base move does not touch.
const rebaseAgentChange = `set -e
printf 'package tally\n\nfunc Count() int { return 1 }\n' > src/tally.go
mkdir -p docs && printf '# Notes\n' > docs/notes.md`

// moveBase commits rel with content to the prepared tree the sandboxes
// are made from, as the default branch moving on would, prepares the
// tree again, and returns the new head.
func (g *dockerGate) moveBase(t *testing.T, rel, content string) string {
	t.Helper()
	dir := g.spec.TreePath
	sandboxtest.WriteFile(t, dir, rel, content)
	sandboxtest.Git(t, dir, "add", "-A")
	sandboxtest.Git(t, dir, "commit", "-q", "-m", "base moves")
	if err := sandbox.PrepareTree(dir); err != nil {
		t.Fatalf("PrepareTree: %v", err)
	}
	head, err := sandbox.HeadCommit(dir)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// TestRebase_DockerApplies: the default branch moved on in a file the
// agent did not touch. The diff applies three-way at the new base, and
// the rebased artifact stages and round-trips in a clean sandbox there,
// as the re-gate will need.
func TestRebase_DockerApplies(t *testing.T) {
	for name, image := range gateImages(t) {
		t.Run(name, func(t *testing.T) {
			g := newDockerGate(t, image)
			art, diff := g.freezeAgent(t, rebaseAgentChange)
			newBase := g.moveBase(t, "README.md", "# Election Tally\n\nMoved on.\n")
			blobs := newTestBlobs(t)
			rebased, err := Rebase(g.h.Ctx, g.fresh(t), blobs, art, diff, newBase)
			if err != nil {
				t.Fatalf("Rebase: %v", err)
			}
			if rebased.Base != newBase || rebased.Hash == art.Hash {
				t.Fatalf("rebased artifact %+v: want a new artifact on %s", rebased, newBase)
			}
			_, rebasedDiff, err := Load(g.h.Ctx, blobs, rebased.Key)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(rebasedDiff), "+func Count() int { return 1 }") || strings.Contains(string(rebasedDiff), "Moved on") {
				t.Fatalf("rebased diff does not carry exactly the agent's change:\n%s", rebasedDiff)
			}
			g.base = newBase
			clean := g.fresh(t)
			reproduced, err := stage(g.h.Ctx, clean, g.spec.TreePath, rebased, rebasedDiff)
			if err != nil || !reproduced {
				t.Fatalf("stage of the rebased artifact = %t, %v; want reproduced", reproduced, err)
			}
			g.assertGated(t, clean, rebasedDiff, "/workspace/"+rebaseDiffName)
		})
	}
}

// TestRebase_DockerConflict: the default branch changed the line the
// agent changed. The three-way apply fails, and Rebase reports a
// conflict, not a broken environment.
func TestRebase_DockerConflict(t *testing.T) {
	g := newDockerGate(t, "")
	art, diff := g.freezeAgent(t, rebaseAgentChange)
	newBase := g.moveBase(t, "src/tally.go", "package tally\n\nfunc Count() int { return 2 }\n")
	_, err := Rebase(g.h.Ctx, g.fresh(t), newTestBlobs(t), art, diff, newBase)
	if !errors.Is(err, ErrRebaseConflict) || errors.Is(err, ErrEnvironment) {
		t.Fatalf("Rebase error = %v, want ErrRebaseConflict", err)
	}
}
