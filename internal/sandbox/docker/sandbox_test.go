package docker_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
	if got := h.out(t, sb, "ls -A"); got != ".git\n.gitignore\n.gitmodules\nREADME.md\nlib\nsrc" {
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
		Command: []string{"/bin/sh", "-c", "sleep 300 & while :; do echo streaming; done"},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut || !bytes.Contains(res.Stdout, []byte("streaming")) {
		t.Fatalf("expected TimedOut with captured output, got timedOut=%v len=%d", res.TimedOut, len(res.Stdout))
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
	time.Sleep(500 * time.Millisecond)
	if _, err := sb.Exec(h.ctx, contracts.ExecRequest{Command: []string{"true"}}); !errors.Is(err, docker.ErrExpired) {
		t.Errorf("Exec past lifetime = %v, want ErrExpired", err)
	}
}

func TestCallerCancellationKillsTheCommand(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))

	ctx, cancel := context.WithTimeout(h.ctx, 500*time.Millisecond)
	defer cancel()
	// The command streams output the whole time, so a buffer read that
	// raced the reader would be caught by the race detector.
	res, err := sb.Exec(ctx, contracts.ExecRequest{Command: []string{"/bin/sh", "-c", "while :; do echo streaming; done"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec = %v, want context deadline", err)
	}
	if !bytes.Contains(res.Stdout, []byte("streaming")) {
		t.Error("output captured before cancellation was lost")
	}
	if got := h.out(t, sb, "ps -o args | grep -c '[e]cho streaming' || true"); got != "0" {
		t.Errorf("cancelled command survived: %s", got)
	}
}

func TestCloseDoesNotWaitForARunningExec(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))

	execErr := make(chan error, 1)
	go func() {
		_, err := sb.Exec(h.ctx, contracts.ExecRequest{Command: []string{"sleep", "300"}, Timeout: time.Minute})
		execErr <- err
	}()
	time.Sleep(500 * time.Millisecond)

	start := time.Now()
	if err := sb.Close(h.ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Close waited %v for the running command", took)
	}
	select {
	case err := <-execErr:
		if !errors.Is(err, docker.ErrClosed) {
			t.Errorf("Exec under Close = %v, want ErrClosed", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Exec did not return after Close")
	}
	if n := len(h.containers(t)); n != 0 {
		t.Errorf("%d containers remain", n)
	}
}

func TestExecRunsBinariesFromWritableMounts(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))

	// A real ELF binary copied into each writable mount, and a script
	// written there, both executed from that mount.
	for _, dir := range []string{"/workspace", "/home/agent", "/tmp"} {
		got := h.out(t, sb, "mkdir -p "+dir+"/bin && cp /bin/busybox "+dir+"/bin/busybox && "+dir+"/bin/busybox echo binary-ok; printf '#!/bin/sh\\necho script-ok\\n' > "+dir+"/run.sh && chmod +x "+dir+"/run.sh && "+dir+"/run.sh")
		if got != "binary-ok\nscript-ok" {
			t.Errorf("%s: %q", dir, got)
		}
	}
	// The workspace is where a toolchain builds and runs tests.
	if err := sb.WriteFile(h.ctx, "scripts/test.sh", []byte("#!/bin/sh\necho tests-pass\n")); err != nil {
		t.Fatal(err)
	}
	if got := h.out(t, sb, "chmod +x scripts/test.sh && ./scripts/test.sh"); got != "tests-pass" {
		t.Errorf("script from the workspace: %q", got)
	}
	if got := h.out(t, sb, "chmod u+s /tmp/bin/busybox && /tmp/bin/busybox id -u"); got != "1000" {
		t.Errorf("setuid should be ignored on the mount: %q", got)
	}
}

func TestReadFileTooLargeIsAPlainError(t *testing.T) {
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

	h.out(t, raw, "dd if=/dev/zero of=big.bin bs=1k count=100 2>/dev/null; echo done")
	if _, err := raw.ReadFile(h.ctx, "big.bin"); !errors.Is(err, docker.ErrFileTooLarge) {
		t.Fatalf("ReadFile = %v, want ErrFileTooLarge", err)
	}
	// Nothing was killed: a process started before the read is still there.
	h.out(t, raw, "sleep 300 >/dev/null 2>&1 & echo started")
	if _, err := raw.ReadFile(h.ctx, "big.bin"); !errors.Is(err, docker.ErrFileTooLarge) {
		t.Fatalf("ReadFile = %v, want ErrFileTooLarge", err)
	}
	if got := h.out(t, raw, "ps -o args | grep -c '^sleep 300' || true"); got != "1" {
		t.Errorf("an oversized read killed other processes: %s sleeps remain", got)
	}
}

func TestCreateFailureLeavesNothingBehind(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(t, contracts.NetworkRegistries)
	// An unreadable file makes the tree upload fail after the network and
	// the container both exist.
	if err := os.Chmod(filepath.Join(spec.TreePath, "README.md"), 0); err != nil {
		t.Fatal(err)
	}

	if _, _, err := h.p.CreateWithInstall(h.ctx, spec, nil); err == nil {
		t.Fatal("CreateWithInstall should fail when the tree cannot be archived")
	}
	if n := len(h.containers(t)); n != 0 {
		t.Errorf("%d containers left behind", n)
	}
	if n := h.networks(t); n != 0 {
		t.Errorf("%d networks left behind", n)
	}
}
