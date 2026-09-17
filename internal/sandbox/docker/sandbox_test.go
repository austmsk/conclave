package docker_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox/docker"
)

func TestLifecycle(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(t, contracts.NetworkNone)
	sb := h.create(t, spec)

	// One sandbox, exercised in order: a diff needs prior writes, and a
	// close needs everything else done.
	t.Run("workspace holds the tree and nothing else", func(t *testing.T) { checkWorkspace(t, h, sb) })
	t.Run("exec reports exit code, streams and duration", func(t *testing.T) { checkExec(t, h, sb) })
	t.Run("exec honours a relative workdir and rejects escapes", func(t *testing.T) { checkWorkDir(t, h, sb) })
	t.Run("write then read round-trips binary content", func(t *testing.T) { checkRoundTrip(t, h, sb) })
	t.Run("read and write errors are explicit", func(t *testing.T) { checkFileErrors(t, h, sb) })
	t.Run("diff covers edits, additions and deletions", func(t *testing.T) { checkDiff(t, h, sb) })
	t.Run("close is idempotent and ends the sandbox", func(t *testing.T) { checkClose(t, h, sb) })
}

func checkWorkspace(t *testing.T, h *harness, sb *docker.Sandbox) {
	if got := h.out(t, sb, "ls -A"); got != ".git\n.gitignore\nREADME.md\nsrc" {
		t.Errorf("workspace listing:\n%s", got)
	}
	if got := h.out(t, sb, "cat src/tally.go"); !strings.Contains(got, "func Count") {
		t.Errorf("tree content missing: %q", got)
	}
}

func checkExec(t *testing.T, h *harness, sb *docker.Sandbox) {
	res := h.sh(t, sb, "echo out; echo err >&2; exit 3")
	if res.ExitCode != 3 || string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" {
		t.Errorf("result = %+v", res)
	}
	if res.TimedOut || res.Duration <= 0 {
		t.Errorf("timed out=%v duration=%v", res.TimedOut, res.Duration)
	}
}

func checkWorkDir(t *testing.T, h *harness, sb *docker.Sandbox) {
	res, err := sb.Exec(h.ctx, contracts.ExecRequest{Command: []string{"pwd"}, WorkDir: "src"})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "/workspace/src" {
		t.Errorf("pwd in src = %q, %v", res.Stdout, err)
	}
	_, err = sb.Exec(h.ctx, contracts.ExecRequest{Command: []string{"pwd"}, WorkDir: "../"})
	if !errors.Is(err, docker.ErrInvalidPath) {
		t.Errorf("workdir escape = %v, want ErrInvalidPath", err)
	}
}

func checkRoundTrip(t *testing.T, h *harness, sb *docker.Sandbox) {
	data := append([]byte("binary\x00\xff\n"), bytes.Repeat([]byte("x"), 300_000)...)
	if err := sb.WriteFile(h.ctx, "deep/new/dir/blob.bin", data); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := sb.ReadFile(h.ctx, "deep/new/dir/blob.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("round trip lost data: got %d bytes want %d", len(got), len(data))
	}
	if err := sb.WriteFile(h.ctx, "deep/new/dir/blob.bin", []byte("short")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, _ := sb.ReadFile(h.ctx, "deep/new/dir/blob.bin"); string(got) != "short" {
		t.Errorf("overwrite left %q", got)
	}
}

func checkFileErrors(t *testing.T, h *harness, sb *docker.Sandbox) {
	if _, err := sb.ReadFile(h.ctx, "missing.txt"); err == nil {
		t.Error("reading a missing file should fail")
	}
	if _, err := sb.ReadFile(h.ctx, "src"); err == nil {
		t.Error("reading a directory should fail")
	}
	if err := sb.WriteFile(h.ctx, "src", []byte("x")); err == nil {
		t.Error("writing over a directory should fail")
	}
	if _, err := sb.ReadFile(h.ctx, ".git/config"); !errors.Is(err, docker.ErrInvalidPath) {
		t.Errorf("reading .git internals = %v, want ErrInvalidPath", err)
	}
}

func checkDiff(t *testing.T, h *harness, sb *docker.Sandbox) {
	writes := map[string][]byte{
		"README.md":       []byte("# Election Tally\n\nEdited.\n"),
		"src/export.go":   []byte("package tally\n"),
		"assets/logo.bin": []byte("\x89PNG\x00\x01\xff"),
	}
	for _, path := range []string{"README.md", "src/export.go", "assets/logo.bin"} {
		if err := sb.WriteFile(h.ctx, path, writes[path]); err != nil {
			t.Fatal(err)
		}
	}
	h.out(t, sb, "rm .gitignore")
	diff, err := sb.Diff(h.ctx)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	for _, want := range []string{"+Edited.", "diff --git a/src/export.go b/src/export.go", "new file mode", "deleted file mode", "a/.gitignore", "assets/logo.bin", "GIT binary patch"} {
		if !strings.Contains(string(diff), want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}
	again, err := sb.Diff(h.ctx)
	if err != nil || !bytes.Equal(again, diff) {
		t.Errorf("Diff is not stable across calls: %v", err)
	}
}

func checkClose(t *testing.T, h *harness, sb *docker.Sandbox) {
	if err := sb.Close(h.ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sb.Close(h.ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := sb.Exec(h.ctx, contracts.ExecRequest{Command: []string{"true"}}); !errors.Is(err, docker.ErrClosed) {
		t.Errorf("Exec after Close = %v, want ErrClosed", err)
	}
	if n := len(h.containers(t)); n != 0 {
		t.Errorf("%d containers remain after Close", n)
	}
}

func TestExecTimeoutKillsTheProcessTree(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))

	start := time.Now()
	res, err := sb.Exec(h.ctx, contracts.ExecRequest{
		Command: []string{"/bin/sh", "-c", "sleep 300 & sleep 300"},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("timeout took %v to enforce", elapsed)
	}
	if got := h.out(t, sb, "ps -o args | grep -c '^sleep 300' || true"); got != "0" {
		t.Errorf("background sleep survived the timeout: %s", got)
	}
	if got := h.out(t, sb, "echo alive"); got != "alive" {
		t.Errorf("sandbox unusable after timeout: %q", got)
	}
}

func TestExecOutputCapKillsTheCommand(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(t, contracts.NetworkNone)
	p, err := docker.New(h.ctx, docker.Config{Namespace: h.namespace, MaxOutputBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	raw, err := p.Create(h.ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close(context.Background()) }()

	_, err = raw.Exec(h.ctx, contracts.ExecRequest{Command: []string{"/bin/sh", "-c", "yes | head -c 10000000; sleep 30"}, Timeout: 30 * time.Second})
	if !errors.Is(err, docker.ErrOutputTooLarge) {
		t.Fatalf("Exec = %v, want ErrOutputTooLarge", err)
	}
	if got := h.out(t, raw, "echo alive"); got != "alive" {
		t.Errorf("sandbox unusable after output cap: %q", got)
	}
}

func TestExecRefusesAfterLifetime(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(t, contracts.NetworkNone)
	spec.Limits.Timeout = 3 * time.Second
	sb := h.create(t, spec)

	res, err := sb.Exec(h.ctx, contracts.ExecRequest{Command: []string{"sleep", "30"}, Timeout: time.Minute})
	if err != nil || !res.TimedOut {
		t.Fatalf("sandbox lifetime should bound a longer request timeout: %+v %v", res, err)
	}
	time.Sleep(time.Until(time.Now().Add(500 * time.Millisecond)))
	if _, err := sb.Exec(h.ctx, contracts.ExecRequest{Command: []string{"true"}}); !errors.Is(err, docker.ErrExpired) {
		t.Errorf("Exec past lifetime = %v, want ErrExpired", err)
	}
}

func TestCallerCancellationKillsTheCommand(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))

	ctx, cancel := context.WithTimeout(h.ctx, 500*time.Millisecond)
	defer cancel()
	_, err := sb.Exec(ctx, contracts.ExecRequest{Command: []string{"sleep", "300"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec = %v, want context deadline", err)
	}
	if got := h.out(t, sb, "ps -o comm | grep -c '^sleep' || true"); got != "1" {
		// One sleep is the init loop's own.
		t.Errorf("cancelled command survived: %s sleeps", got)
	}
}

func TestCreateFailureLeavesNothingBehind(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(t, contracts.NetworkRegistries)
	// Below the daemon's minimum, so ContainerCreate fails after the
	// network has already been created.
	spec.Limits.MemoryBytes = 1 << 20

	if _, err := h.p.Create(h.ctx, spec); err == nil {
		t.Fatal("Create should fail for a memory limit the daemon rejects")
	}
	if n := len(h.containers(t)); n != 0 {
		t.Errorf("%d containers left behind", n)
	}
	if n := h.networks(t); n != 0 {
		t.Errorf("%d networks left behind", n)
	}
}
