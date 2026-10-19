package docker_test

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox/docker"
	"github.com/austmsk/conclave/internal/sandbox/docker/dockertest"
)

// harness wraps dockertest.Harness with the short names these tests use.
type harness struct {
	*dockertest.Harness
	ctx context.Context
	p   *docker.Provider
	cli *client.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := dockertest.New(t)
	return &harness{Harness: h, ctx: h.Ctx, p: h.Provider, cli: h.Client}
}

func (h *harness) spec(t *testing.T, network contracts.NetworkMode) contracts.SandboxSpec {
	t.Helper()
	return h.Spec(t, network)
}

func (h *harness) create(t *testing.T, spec contracts.SandboxSpec) *docker.Sandbox {
	t.Helper()
	return h.Create(t, spec)
}

func (h *harness) sh(t *testing.T, sb contracts.Sandbox, script string) contracts.ExecResult {
	t.Helper()
	return h.Sh(t, sb, script)
}

func (h *harness) out(t *testing.T, sb contracts.Sandbox, script string) string {
	t.Helper()
	return h.Out(t, sb, script)
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
		Filters: make(client.Filters).Add("label", "conclave.sandbox.namespace="+h.Namespace),
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.Items
}

func (h *harness) networks(t *testing.T) int {
	t.Helper()
	res, err := h.cli.NetworkList(h.ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", "conclave.sandbox.namespace="+h.Namespace),
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(res.Items)
}
