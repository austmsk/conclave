package docker_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/docker"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

// testImage is alpine/git v2.49.1, pinned by its multi-platform index
// digest so the same reference resolves on arm64 and amd64 daemons. It
// ships sh, git, and the busybox tools the attack tests use.
const testImage = "docker.io/alpine/git@sha256:c0280cf9572316299b08544065d3bf35db65043d5e3963982ec50647d2746e26"

func image() string {
	if v := os.Getenv("CONCLAVE_SANDBOX_TEST_IMAGE"); v != "" {
		return v
	}
	return testImage
}

// harness holds one provider per test in a namespace of its own, so
// leftovers are visible only to that test and are reaped when it ends.
type harness struct {
	ctx       context.Context
	p         *docker.Provider
	cli       *client.Client
	namespace string
}

// newHarness connects to Docker. Without Docker the suite fails rather
// than silently passing; pass -short to skip it.
func newHarness(t *testing.T) *harness {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: run without -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	var b [4]byte
	_, _ = rand.Read(b[:])
	ns := "test-" + hex.EncodeToString(b[:])
	p, err := docker.New(ctx, docker.Config{Namespace: ns})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{ctx: ctx, p: p, cli: cli, namespace: ns}
	t.Cleanup(func() {
		bg, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := p.Reap(bg, 0); err != nil {
			t.Errorf("reaping test namespace: %v", err)
		}
		_ = p.Close()
		_ = cli.Close()
	})
	return h
}

func (h *harness) spec(t *testing.T, network contracts.NetworkMode) contracts.SandboxSpec {
	t.Helper()
	tree, _ := sandboxtest.NewRepo(t)
	if err := sandbox.PrepareTree(tree); err != nil {
		t.Fatal(err)
	}
	return contracts.SandboxSpec{
		Image:    image(),
		TreePath: tree,
		Env:      map[string]string{"TALLY_API_KEY": "canary_tally_api_key", "GOFLAGS": "-mod=mod"},
		Network:  network,
		Limits: contracts.ResourceLimits{
			MemoryBytes: 256 << 20,
			CPUShares:   512,
			DiskBytes:   32 << 20,
			MaxProcs:    64,
			Timeout:     2 * time.Minute,
		},
	}
}

func (h *harness) create(t *testing.T, spec contracts.SandboxSpec) *docker.Sandbox {
	t.Helper()
	sb, err := h.p.Create(h.ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return sb.(*docker.Sandbox)
}

// sh runs a shell script in the sandbox and returns its result.
func (h *harness) sh(t *testing.T, sb contracts.Sandbox, script string) contracts.ExecResult {
	t.Helper()
	res, err := sb.Exec(h.ctx, contracts.ExecRequest{Command: []string{"/bin/sh", "-c", script}, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("exec %q: %v", script, err)
	}
	return res
}

// out runs a script that must succeed and returns trimmed stdout.
func (h *harness) out(t *testing.T, sb contracts.Sandbox, script string) string {
	t.Helper()
	res := h.sh(t, sb, script)
	if res.ExitCode != 0 {
		t.Fatalf("exec %q exited %d: %s", script, res.ExitCode, res.Stderr)
	}
	return strings.TrimSpace(string(res.Stdout))
}

func (h *harness) inspect(t *testing.T, sb *docker.Sandbox) container.InspectResponse {
	t.Helper()
	res, err := h.cli.ContainerInspect(h.ctx, sb.ID(), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return res.Container
}

// containers lists every container in the harness namespace.
func (h *harness) containers(t *testing.T) []container.Summary {
	t.Helper()
	res, err := h.cli.ContainerList(h.ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", "conclave.sandbox.namespace="+h.namespace),
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.Items
}

func (h *harness) networks(t *testing.T) int {
	t.Helper()
	res, err := h.cli.NetworkList(h.ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", "conclave.sandbox.namespace="+h.namespace),
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(res.Items)
}
