package gate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/docker/dockertest"
)

// java21ImageEnv names a digest-pinned build of deploy/images/java21, for
// running the Docker gate tests against the image ETS is gated in, e.g.
// conclave-sandbox-java21@sha256:<digest>. CI does not build that image,
// so without the variable those runs skip; the busybox runs always go.
const java21ImageEnv = "CONCLAVE_SANDBOX_JAVA21_IMAGE"

// dockerGate is one Docker sandbox over a fresh prepared fixture tree, as
// the agent phase would leave it.
type dockerGate struct {
	h    *dockertest.Harness
	spec contracts.SandboxSpec
	base string
	sb   contracts.Sandbox
}

// newDockerGate starts a sandbox from image, or the test image when image
// is empty. Without Docker it skips locally and fails in CI.
func newDockerGate(t *testing.T, image string) *dockerGate {
	t.Helper()
	h := dockertest.New(t)
	spec := h.Spec(t, contracts.NetworkNone)
	if image != "" {
		spec.Image = image
	}
	base, err := sandbox.HeadCommit(spec.TreePath)
	if err != nil {
		t.Fatal(err)
	}
	return &dockerGate{h: h, spec: spec, base: base, sb: h.Create(t, spec)}
}

// fresh starts a second sandbox over the same prepared tree: clean_room.
func (g *dockerGate) fresh(t *testing.T) contracts.Sandbox {
	t.Helper()
	return g.h.Create(t, g.spec)
}

// gateImages are the images the Docker gate tests run in: the pinned
// busybox test image, and the java21 image when java21ImageEnv names one.
func gateImages(t *testing.T) map[string]string {
	t.Helper()
	images := map[string]string{"test-image": ""}
	if img := os.Getenv(java21ImageEnv); img != "" {
		images["java21"] = img
	} else {
		t.Logf("%s unset: not running against deploy/images/java21", java21ImageEnv)
	}
	return images
}

// agentChange is an edit the agent phase might leave: a modified file, a
// new file, a deleted file, a new executable and a binary file, so the
// round trip covers modes and binary patches.
const agentChange = `set -e
printf 'package tally\n\nfunc Count() int { return 1 }\n' > src/tally.go
mkdir -p docs && printf '# Notes\n' > docs/notes.md
rm README.md
printf '#!/bin/sh\necho hi\n' > run.sh && chmod +x run.sh
printf '\000\001\002\377binary' > data.bin`

// freezeAgent applies script in the agent's sandbox and freezes the
// result, returning the artifact and its diff as loaded from the store.
func (g *dockerGate) freezeAgent(t *testing.T, script string) (Artifact, []byte) {
	t.Helper()
	g.h.Out(t, g.sb, script)
	blobs := newTestBlobs(t)
	art, err := Freeze(g.h.Ctx, g.sb, blobs, testRepoID, g.base)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	loaded, diff, err := Load(g.h.Ctx, blobs, art.Key)
	if err != nil || loaded != art {
		t.Fatalf("Load = %+v, %v; want %+v", loaded, err, art)
	}
	return art, diff
}

// isolatedDiff re-diffs a sandbox's workspace the way staging's round
// trip does, so a test can check the staged tree without the agent's
// home configuration, which staging keeps, colouring the answer.
func (g *dockerGate) isolatedDiff(t *testing.T, sb contracts.Sandbox) []byte {
	t.Helper()
	cmd := append(append([]string{}, isolatedGit...), "sh", "-c", roundTripScript, "sh", g.base)
	res, err := sb.Exec(g.h.Ctx, contracts.ExecRequest{Command: cmd})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("isolated diff: exit %d, %v: %s", res.ExitCode, err, res.Stderr)
	}
	return res.Stdout
}

// assertGated checks that sb holds exactly base plus diff: the re-diff
// equals the recorded bytes, and none of absent exists anywhere the agent
// could have left it.
func (g *dockerGate) assertGated(t *testing.T, sb contracts.Sandbox, diff []byte, absent ...string) {
	t.Helper()
	if got := g.isolatedDiff(t, sb); !bytes.Equal(got, diff) {
		t.Errorf("gated tree differs from base + recorded diff:\n got: %q\nwant: %q", got, diff)
	}
	for _, p := range absent {
		if res := g.h.Sh(t, sb, "test ! -e '"+p+"'"); res.ExitCode != 0 {
			t.Errorf("%s survived staging", p)
		}
	}
}

// TestStage_DockerRoundTrip: an agent's change frozen in one sandbox
// reproduces exactly when staged into the same sandbox and into a fresh
// one, in every gate image.
func TestStage_DockerRoundTrip(t *testing.T) {
	for name, image := range gateImages(t) {
		t.Run(name, func(t *testing.T) {
			g := newDockerGate(t, image)
			art, diff := g.freezeAgent(t, agentChange)
			if !bytes.Contains(diff, []byte("GIT binary patch")) || !bytes.Contains(diff, []byte("new file mode 100755")) {
				t.Fatalf("fixture change lacks a binary patch or a mode: %s", diff)
			}
			for mode, sb := range map[string]contracts.Sandbox{"reset_in_place": g.sb, "clean_room": g.fresh(t)} {
				reproduced, err := stage(g.h.Ctx, sb, g.spec.TreePath, art, diff)
				if err != nil || !reproduced {
					t.Fatalf("%s: stage = %t, %v; want reproduced", mode, reproduced, err)
				}
				g.assertGated(t, sb, diff, "/tmp/conclave-stage.tar", "/workspace/.conclave-stage.tar", "/workspace/.conclave-stage.diff")
				if got := g.h.Out(t, sb, "stat -c %a run.sh"); got != "755" {
					t.Errorf("%s: run.sh mode %s, want 755", mode, got)
				}
			}
		})
	}
}

// TestStage_DockerDiffDoesNotApply: a recorded diff whose preimage is not
// the base fails to reproduce, as the attempt's own failure.
func TestStage_DockerDiffDoesNotApply(t *testing.T) {
	g := newDockerGate(t, "")
	diff := []byte("diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-# Not The Base\n+changed\n")
	reproduced, err := stage(g.h.Ctx, g.sb, g.spec.TreePath, testArtifact(g.base, diff), diff)
	if err != nil || reproduced {
		t.Fatalf("stage = %t, %v; want not reproduced and no error", reproduced, err)
	}
}

// TestTestsExitZeroWithoutReportFails: a test command that exits 0 and
// writes no report, as System.exit(0) in test code would, fails the tests
// gate.
func TestTestsExitZeroWithoutReportFails(t *testing.T) {
	g := newDockerGate(t, "")
	cmds := Commands{Build: []string{"true"}, Lint: []string{"true"}, Test: []string{"sh", "-c", "exit 0"}}
	got, err := runCommands(g.h.Ctx, g.sb, newTestBlobs(t), testRepoID, cmds)
	if err != nil {
		t.Fatal(err)
	}
	assertVerdicts(t, got, []verdict{
		{Gate: contracts.GateBuild, Passed: true, Detail: "exit 0"},
		{Gate: contracts.GateLint, Passed: true, Detail: "exit 0"},
		{Gate: contracts.GateTests, Detail: "exit 0 but no JUnit report"},
	})
}

// TestRunCommands_DockerReport: the tests gate reads every report file
// the JUnit console launcher writes, in every gate image, and a command
// the image lacks is the environment's failure.
func TestRunCommands_DockerReport(t *testing.T) {
	writeReports := `set -e
mkdir -p ` + JUnitDir + `/nested
printf '%s' '` + jupiterReport + `' > ` + JUnitDir + `/TEST-junit-jupiter.xml
printf '%s' '` + vintageReport + `' > ` + JUnitDir + `/nested/TEST-junit-vintage.xml
ln -s /etc/passwd ` + JUnitDir + `/TEST-link.xml`
	for name, image := range gateImages(t) {
		t.Run(name, func(t *testing.T) {
			g := newDockerGate(t, image)
			cmds := Commands{Build: []string{"true"}, Lint: []string{"true"}, Test: []string{"sh", "-c", writeReports}}
			got, err := runCommands(g.h.Ctx, g.sb, newTestBlobs(t), testRepoID, cmds)
			if err != nil {
				t.Fatal(err)
			}
			assertVerdicts(t, got, []verdict{
				{Gate: contracts.GateBuild, Passed: true, Detail: "exit 0"},
				{Gate: contracts.GateLint, Passed: true, Detail: "exit 0"},
				{Gate: contracts.GateTests, Passed: true, Detail: "2 tests ran, 0 failed, 1 skipped"},
			})

			cmds.Build = []string{"sh", "-c", "conclave-no-such-tool"}
			if _, err := runCommands(g.h.Ctx, g.sb, newTestBlobs(t), testRepoID, cmds); !errors.Is(err, ErrEnvironment) {
				t.Errorf("missing build tool: err = %v, want ErrEnvironment", err)
			}
		})
	}
}

// TestStage_DockerCancelled: cancelling the caller's context mid-staging
// is reported as cancellation, not as a broken environment.
func TestStage_DockerCancelled(t *testing.T) {
	g := newDockerGate(t, "")
	art, diff := g.freezeAgent(t, agentChange)
	ctx, cancel := context.WithCancel(g.h.Ctx)
	cancel()
	_, err := stage(ctx, g.sb, g.spec.TreePath, art, diff)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrEnvironment) {
		t.Fatalf("stage error = %v, want the context's error", err)
	}
}
