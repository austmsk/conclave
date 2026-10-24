package gate

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

// preparedTree returns a prepared fixture tree and its HEAD.
func preparedTree(t *testing.T) (string, string) {
	t.Helper()
	tree, head := sandboxtest.NewRepo(t)
	if err := sandbox.PrepareTree(tree); err != nil {
		t.Fatal(err)
	}
	return tree, head
}

// stagingSandbox scripts a fakeSandbox through staging. Each field is the
// exit code of one step; roundTrip is what the re-diff prints. failOn,
// when set, makes the first Exec whose command contains it fail with
// failErr.
type stagingSandbox struct {
	wipeExit, extractExit, moveExit, applyExit, diffExit int
	roundTrip                                            []byte
	failOn                                               string
	failErr                                              error
}

func (s stagingSandbox) exec(req contracts.ExecRequest) (contracts.ExecResult, error) {
	cmd := strings.Join(req.Command, " ")
	if s.failOn != "" && strings.Contains(cmd, s.failOn) {
		return contracts.ExecResult{}, s.failErr
	}
	switch {
	case strings.Contains(cmd, "rm -rf -- {}"):
		return contracts.ExecResult{ExitCode: s.wipeExit}, nil
	case strings.Contains(cmd, "tar -x"):
		return contracts.ExecResult{ExitCode: s.extractExit}, nil
	case req.Command[0] == "mv":
		return contracts.ExecResult{ExitCode: s.moveExit}, nil
	case strings.Contains(cmd, "git apply"):
		return contracts.ExecResult{ExitCode: s.applyExit}, nil
	case strings.Contains(cmd, "git diff"):
		return contracts.ExecResult{ExitCode: s.diffExit, Stdout: s.roundTrip}, nil
	}
	return contracts.ExecResult{ExitCode: 99}, errors.New("unexpected command " + cmd)
}

// TestStage_RoundTrip: staging wipes, uploads the prepared tree and the
// recorded diff, applies exactly the recorded bytes with git isolated,
// and re-diffs without textconv. A re-diff equal to the recorded bytes
// reproduces.
func TestStage_RoundTrip(t *testing.T) {
	tree, head := preparedTree(t)
	art := testArtifact(head, sampleDiff)
	sb := newFakeSandbox("gating")
	sb.exec = stagingSandbox{roundTrip: sampleDiff}.exec

	reproduced, err := stage(context.Background(), sb, tree, art, sampleDiff)
	if err != nil || !reproduced {
		t.Fatalf("stage = %t, %v; want reproduced", reproduced, err)
	}
	if got := sb.writes[stageDiffName]; !bytes.Equal(got, sampleDiff) {
		t.Errorf("uploaded diff = %q, want the recorded bytes", got)
	}
	names := tarNames(t, sb.writes[stageTarName])
	for _, want := range []string{"README.md", ".git/HEAD", "src/tally.go", ".git/modules/lib/HEAD"} {
		if !slices.Contains(names, want) {
			t.Errorf("uploaded tree lacks %s; has %v", want, names)
		}
	}
	order := []string{"rm -rf -- {}", "tar -x", "git apply", "git diff"}
	for i := 1; i < len(order); i++ {
		if sb.ran(order[i-1]) >= sb.ran(order[i]) {
			t.Errorf("%q did not run before %q: %q", order[i-1], order[i], sb.commands())
		}
	}
	isolated := strings.Join(isolatedGit, " ") + " "
	for _, step := range []string{"git apply --binary " + stageDiffPath, "--no-textconv"} {
		i := sb.ran(step)
		if i < 0 || !strings.HasPrefix(sb.commands()[i], isolated) {
			t.Errorf("%q not run under %q: %q", step, isolated, sb.commands())
		}
	}
	if !strings.HasSuffix(sb.commands()[sb.ran("git diff")], " "+head) {
		t.Errorf("re-diff not taken against the base: %q", sb.commands()[sb.ran("git diff")])
	}
}

// tarNames lists the entries of a tar archive.
func tarNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatalf("reading uploaded tree: %v", err)
		}
		names = append(names, strings.TrimSuffix(hdr.Name, "/"))
	}
}

// TestStage_Outcomes: a diff that does not apply or does not round-trip
// is the attempt's own failure (false, nil); anything that stops staging
// itself is ErrEnvironment.
func TestStage_Outcomes(t *testing.T) {
	changed := append(append([]byte(nil), sampleDiff...), "+more\n"...)
	tests := []struct {
		name           string
		script         stagingSandbox
		wantReproduced bool
		wantEnv        bool
		wantNoRediff   bool
	}{
		{name: "reproduces", script: stagingSandbox{roundTrip: sampleDiff}, wantReproduced: true},
		{name: "round trip differs", script: stagingSandbox{roundTrip: changed}},
		{name: "round trip empty", script: stagingSandbox{}},
		{name: "apply refused", script: stagingSandbox{applyExit: 1, roundTrip: sampleDiff}, wantNoRediff: true},
		{name: "round trip output past limit", script: stagingSandbox{failOn: "git diff", failErr: sandbox.ErrOutputTooLarge}},
		{name: "wipe leaves files", script: stagingSandbox{wipeExit: 1}, wantEnv: true},
		{name: "extract fails", script: stagingSandbox{extractExit: 2}, wantEnv: true},
		{name: "move fails", script: stagingSandbox{moveExit: 1}, wantEnv: true},
		{name: "git missing", script: stagingSandbox{applyExit: 127}, wantEnv: true},
		{name: "re-diff fails", script: stagingSandbox{diffExit: 128}, wantEnv: true},
		{name: "wipe exec error", script: stagingSandbox{failOn: "rm -rf -- {}", failErr: errors.New("sandbox killed")}, wantEnv: true},
		{name: "apply exec error", script: stagingSandbox{failOn: "git apply", failErr: errors.New("sandbox expired")}, wantEnv: true},
		{name: "apply output past limit", script: stagingSandbox{failOn: "git apply", failErr: sandbox.ErrOutputTooLarge}, wantEnv: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree, head := preparedTree(t)
			sb := newFakeSandbox("gating")
			sb.exec = tt.script.exec
			reproduced, err := stage(context.Background(), sb, tree, testArtifact(head, sampleDiff), sampleDiff)
			if reproduced != tt.wantReproduced {
				t.Errorf("reproduced = %t, want %t", reproduced, tt.wantReproduced)
			}
			if tt.wantEnv != errors.Is(err, ErrEnvironment) || (!tt.wantEnv && err != nil) {
				t.Errorf("err = %v, want environment error: %t", err, tt.wantEnv)
			}
			if tt.wantNoRediff && sb.ran("git diff") >= 0 {
				t.Errorf("re-diffed after a refused apply: %q", sb.commands())
			}
		})
	}
}

// TestStage_RefusesBeforeTouchingSandbox: a prepared tree that is not at
// the artifact's base, is not prepared, or uses a staging name, and a
// diff that is not the artifact's, are environment failures found on the
// worker before the sandbox is wiped.
func TestStage_RefusesBeforeTouchingSandbox(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, tree, head string) (Artifact, []byte)
	}{
		{name: "head is not base", setup: func(_ *testing.T, _, _ string) (Artifact, []byte) {
			return testArtifact(baseA, sampleDiff), sampleDiff
		}},
		{name: "diff is not the artifact's", setup: func(_ *testing.T, _, head string) (Artifact, []byte) {
			return testArtifact(head, sampleDiff), append([]byte("x"), sampleDiff...)
		}},
		{name: "tree not prepared", setup: func(t *testing.T, tree, head string) (Artifact, []byte) {
			sandboxtest.WriteFile(t, tree, ".git/FETCH_HEAD", "canary_fetch_head\n")
			return testArtifact(head, sampleDiff), sampleDiff
		}},
		{name: "tree uses a staging name", setup: func(t *testing.T, tree, head string) (Artifact, []byte) {
			sandboxtest.WriteFile(t, tree, stageTarName, "x")
			return testArtifact(head, sampleDiff), sampleDiff
		}},
		{name: "tree missing", setup: func(t *testing.T, tree, head string) (Artifact, []byte) {
			if err := os.RemoveAll(filepath.Join(tree, ".git")); err != nil {
				t.Fatal(err)
			}
			return testArtifact(head, sampleDiff), sampleDiff
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree, head := preparedTree(t)
			art, diff := tt.setup(t, tree, head)
			sb := newFakeSandbox("gating")
			sb.exec = stagingSandbox{roundTrip: diff}.exec
			reproduced, err := stage(context.Background(), sb, tree, art, diff)
			if reproduced || !errors.Is(err, ErrEnvironment) {
				t.Fatalf("stage = %t, %v; want an environment error", reproduced, err)
			}
			if len(sb.commands()) != 0 || len(sb.writeOrder) != 0 {
				t.Errorf("sandbox touched before the refusal: %q, writes %q", sb.commands(), sb.writeOrder)
			}
		})
	}
}

// TestStage_UploadFails: a WriteFile that fails is the environment's.
func TestStage_UploadFails(t *testing.T) {
	tree, head := preparedTree(t)
	sb := newFakeSandbox("gating")
	sb.exec = stagingSandbox{roundTrip: sampleDiff}.exec
	sb.writeErr = errors.New("writing \".conclave-stage.tar\": exit 1: no space left on device")
	reproduced, err := stage(context.Background(), sb, tree, testArtifact(head, sampleDiff), sampleDiff)
	if reproduced || !errors.Is(err, ErrEnvironment) {
		t.Fatalf("stage = %t, %v; want an environment error", reproduced, err)
	}
}

// TestStage_Cancelled: a cancelled context is reported as such, never as
// a broken environment, so the caller does not mark the attempt errored
// for its own cancellation.
func TestStage_Cancelled(t *testing.T) {
	tree, head := preparedTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	sb := newFakeSandbox("gating")
	sb.exec = func(req contracts.ExecRequest) (contracts.ExecResult, error) {
		if strings.Contains(strings.Join(req.Command, " "), "git apply") {
			cancel()
			return contracts.ExecResult{}, errors.Join(context.Canceled, errors.New("killing after cancel"))
		}
		return stagingSandbox{roundTrip: sampleDiff}.exec(req)
	}
	_, err := stage(ctx, sb, tree, testArtifact(head, sampleDiff), sampleDiff)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrEnvironment) {
		t.Fatalf("stage error = %v, want context.Canceled and not ErrEnvironment", err)
	}
}

// TestStage_EmptyDiffSkipsApply: git apply refuses empty input, so an
// empty diff goes straight to the round trip.
func TestStage_EmptyDiffSkipsApply(t *testing.T) {
	tree, head := preparedTree(t)
	sb := newFakeSandbox("gating")
	sb.exec = stagingSandbox{applyExit: 1}.exec
	reproduced, err := stage(context.Background(), sb, tree, testArtifact(head, nil), nil)
	if err != nil || !reproduced {
		t.Fatalf("stage = %t, %v; want reproduced", reproduced, err)
	}
	if sb.ran("git apply") >= 0 {
		t.Errorf("applied an empty diff: %q", sb.commands())
	}
}
