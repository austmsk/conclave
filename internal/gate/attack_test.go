package gate

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// These tests attack freezing and staging through the Docker backend the
// worker runs. Each plays the agent inside the sandbox: whatever it can do
// with run_command, it does here with Exec.

// TestAttackFreezeBackgroundWriter: a process the agent leaves running
// cannot change the artifact after the agent phase ends. The provider
// kills everything after each command, so both of Freeze's reads agree
// and the workspace stays as frozen.
func TestAttackFreezeBackgroundWriter(t *testing.T) {
	writers := map[string]string{
		"background loop": `(while :; do echo canary_late_write >> README.md; sleep 0.05; done) >/dev/null 2>&1 &
sleep 0.3`,
		"double fork": `( (while :; do echo canary_late_write >> src/tally.go; sleep 0.05; done) >/dev/null 2>&1 & ) &
sleep 0.3`,
		"trap ignoring": `(trap '' HUP TERM INT; while :; do echo canary_late_write >> docs.md; sleep 0.05; done) >/dev/null 2>&1 &
sleep 0.3`,
	}
	for name, script := range writers {
		t.Run(name, func(t *testing.T) {
			g := newDockerGate(t, "")
			art, diff := g.freezeAgent(t, script)
			time.Sleep(500 * time.Millisecond)
			later, err := g.sb.Diff(g.h.Ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(later, diff) {
				t.Fatalf("workspace changed after Freeze:\nfrozen: %q\n later: %q", diff, later)
			}
			if HashArtifact(g.base, later) != art.Hash {
				t.Errorf("artifact hash no longer matches the workspace")
			}
		})
	}
}

// postFreezeMutation is what an agent process would do after the freeze
// if anything let it run: change a tracked file, add an untracked one,
// plant an ignored one, and leave a report where the tests gate looks.
const postFreezeMutation = `set -e
echo canary_after_freeze >> src/tally.go
echo canary_after_freeze > evil.txt
mkdir -p node_modules && echo canary_after_freeze > node_modules/evil.js
mkdir -p ` + JUnitDir + ` && echo '<testsuite><testcase/></testsuite>' > ` + JUnitDir + `/TEST-evil.xml`

// TestAttackPostFreezeMutationIgnored: in both modes, what is gated is
// base plus the recorded diff, whatever the agent's workspace became
// after the freeze. reset_in_place stages over the mutated workspace;
// clean_room never sees it.
func TestAttackPostFreezeMutationIgnored(t *testing.T) {
	for _, mode := range []contracts.GatingMode{contracts.GatingResetInPlace, contracts.GatingCleanRoom} {
		t.Run(string(mode), func(t *testing.T) {
			g := newDockerGate(t, "")
			art, diff := g.freezeAgent(t, agentChange)
			g.h.Out(t, g.sb, postFreezeMutation)
			target := g.sb
			if mode == contracts.GatingCleanRoom {
				target = g.fresh(t)
			}
			reproduced, err := stage(g.h.Ctx, target, g.spec.TreePath, art, diff)
			if err != nil || !reproduced {
				t.Fatalf("stage = %t, %v; want reproduced", reproduced, err)
			}
			g.assertGated(t, target, diff, "evil.txt", "node_modules/evil.js", JUnitDir)
			// The provider's own Diff, which is what ticket 9 pushes from,
			// agrees with the recorded bytes too.
			pushed, err := target.Diff(g.h.Ctx)
			if err != nil || !bytes.Equal(pushed, diff) {
				t.Errorf("Diff after staging = %q, %v; want the recorded diff", pushed, err)
			}
			if out := g.h.Out(t, target, "cat src/tally.go"); strings.Contains(out, "canary_after_freeze") {
				t.Errorf("post-freeze edit was gated: %q", out)
			}
		})
	}
}

// agentGitState is git state the agent can leave in its sandbox to make
// staging lie or fail: a skip-worktree bit hiding an edit, fsmonitor
// hooks in the repository and global config, whitespace rules that would
// rewrite or refuse the recorded diff on apply, a prefix change that
// would alter the re-diff, a planted ignored file, a planted file in
// /tmp, and directories the agent made unreadable.
const agentGitState = `set -e
git update-index --skip-worktree src/tally.go
echo canary_hidden_edit >> src/tally.go
printf '#!/bin/sh\necho canary_fsmonitor > /home/agent/pwned\n' > /home/agent/hook.sh
chmod +x /home/agent/hook.sh
git config core.fsmonitor /home/agent/hook.sh
git config apply.whitespace error
git config --global core.fsmonitor /home/agent/hook.sh
git config --global apply.whitespace fix
git config --global diff.noprefix true
mkdir -p node_modules && echo canary_planted > node_modules/planted.js
echo canary_planted > /tmp/planted
mkdir -p locked/inner && echo canary_locked > locked/inner/f
chmod 000 locked/inner locked`

// changeWithWhitespace adds lines with trailing whitespace, which
// apply.whitespace would refuse or strip.
const changeWithWhitespace = `set -e
printf 'package tally\n\nfunc Count() int { return 1 }   \n\t\n' > src/tally.go`

// TestResetInPlaceIgnoresAgentGitState: staging into the agent's own
// sandbox wipes and re-streams .git and isolates git configuration, so
// nothing the agent configured or planted survives or alters the result.
func TestResetInPlaceIgnoresAgentGitState(t *testing.T) {
	g := newDockerGate(t, "")
	art, diff := g.freezeAgent(t, changeWithWhitespace)
	if !bytes.Contains(diff, []byte("   \n")) {
		t.Fatalf("fixture diff lacks trailing whitespace: %q", diff)
	}
	g.h.Out(t, g.sb, agentGitState)

	reproduced, err := stage(g.h.Ctx, g.sb, g.spec.TreePath, art, diff)
	if err != nil || !reproduced {
		t.Fatalf("stage = %t, %v; want reproduced", reproduced, err)
	}
	// /home/agent is kept by design, so the hook's marker is the proof
	// the hook never ran during staging.
	g.assertGated(t, g.sb, diff, "/home/agent/pwned", "node_modules/planted.js", "/tmp/planted", "locked")
	if flags := g.h.Out(t, g.sb, "git ls-files -v src/tally.go"); !strings.HasPrefix(flags, "H ") {
		t.Errorf("skip-worktree survived staging: %q", flags)
	}
	if cfg := g.h.Out(t, g.sb, "cat .git/config"); strings.Contains(cfg, "fsmonitor") || strings.Contains(cfg, "whitespace") {
		t.Errorf("agent's repository config survived staging:\n%s", cfg)
	}
}

// TestAttackTextconvDiffIsWhatIsGated: an agent that configures a
// textconv driver or a clean filter makes Diff show something other than
// the workspace. The recorded bytes, not the workspace, are what staging
// applies and gates, so the hidden content never reaches a gate: the
// gated file is what the diff says. A textconv diff carries the real blob
// IDs of the hidden content, so its round trip also fails; a clean filter
// lies consistently and reproduces, as harmless as any other diff.
func TestAttackTextconvDiffIsWhatIsGated(t *testing.T) {
	tests := []struct {
		name           string
		config         string
		wantReproduced bool
	}{
		{name: "textconv", config: `echo 'README.md diff=lie' > .git/info/attributes
git config diff.lie.textconv 'sed s/canary_hidden/shown/'`},
		{name: "clean filter", config: `echo 'README.md filter=lie' > .git/info/attributes
git config filter.lie.clean 'sed s/canary_hidden/shown/'`, wantReproduced: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newDockerGate(t, "")
			art, diff := g.freezeAgent(t, "set -e\nprintf 'canary_hidden\\n' > README.md\n"+tt.config)
			if bytes.Contains(diff, []byte("canary_hidden")) || !bytes.Contains(diff, []byte("+shown\n")) {
				t.Fatalf("Diff did not lie, so the attack is not exercised: %q", diff)
			}
			reproduced, err := stage(g.h.Ctx, g.sb, g.spec.TreePath, art, diff)
			if err != nil || reproduced != tt.wantReproduced {
				t.Fatalf("stage = %t, %v; want %t, nil", reproduced, err, tt.wantReproduced)
			}
			if got := g.h.Out(t, g.sb, "cat README.md"); got != "shown" {
				t.Errorf("gated README.md = %q, want the recorded content %q", got, "shown")
			}
		})
	}
}

// TestAttackPlantedReportIgnored: a passing JUnit report the agent plants
// in the report directory is wiped by staging, so a test command that
// writes nothing still fails the tests gate.
func TestAttackPlantedReportIgnored(t *testing.T) {
	g := newDockerGate(t, "")
	plant := `set -e
mkdir -p ` + JUnitDir + `
printf '%s' '` + passingReport + `' > ` + JUnitDir + `/TEST-planted.xml
echo ok > src/planted_marker.txt`
	art, diff := g.freezeAgent(t, plant)
	reproduced, err := stage(g.h.Ctx, g.sb, g.spec.TreePath, art, diff)
	if err != nil || !reproduced {
		t.Fatalf("stage = %t, %v; want reproduced", reproduced, err)
	}
	if res := g.h.Sh(t, g.sb, "test ! -e "+JUnitDir); res.ExitCode != 0 {
		t.Fatalf("planted report survived staging")
	}
	cmds := Commands{Build: []string{"true"}, Lint: []string{"true"}, Test: []string{"true"}}
	got, err := runCommands(g.h.Ctx, g.sb, newTestBlobs(t), testRepoID, cmds)
	if err != nil {
		t.Fatal(err)
	}
	if got[2].Passed || got[2].Detail != "exit 0 but no JUnit report" {
		t.Errorf("tests verdict = %+v, want a failure for the missing report", got[2])
	}
}
