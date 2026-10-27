package docker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox/docker"
	"github.com/austmsk/conclave/internal/sandbox/docker/dockertest"
)

// createUnclosed creates a sandbox through the provider without
// registering a Close, as a worker that died mid-attempt would leave it,
// so only Release (or the harness's final Reap) removes it. A networked
// spec goes through CreateWithInstall, which leaves the sandbox's own
// network in place, disconnected, until removal.
func (h *harness) createUnclosed(t *testing.T, network contracts.NetworkMode) contracts.Sandbox {
	t.Helper()
	spec := h.spec(t, network)
	if network == contracts.NetworkRegistries {
		sb, _, err := h.p.CreateWithInstall(h.ctx, spec, []contracts.ExecRequest{{Command: []string{"true"}}})
		if err != nil {
			t.Fatalf("CreateWithInstall: %v", err)
		}
		return sb
	}
	sb, err := h.p.Create(h.ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return sb
}

func TestReleaseRemovesContainerAndNetwork(t *testing.T) {
	cases := []struct {
		name     string
		network  contracts.NetworkMode
		networks int // the sandbox's own networks before Release
	}{
		{"without network", contracts.NetworkNone, 0},
		{"after an install phase", contracts.NetworkRegistries, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			sb := h.createUnclosed(t, tc.network)
			if n := h.networks(t); n != tc.networks {
				t.Fatalf("before Release: %d networks, want %d", n, tc.networks)
			}
			if err := h.p.Release(h.ctx, sb.ID()); err != nil {
				t.Fatalf("Release: %v", err)
			}
			if left := h.containers(t); len(left) != 0 {
				t.Errorf("containers remain after Release: %v", left)
			}
			if n := h.networks(t); n != 0 {
				t.Errorf("%d networks remain after Release", n)
			}
		})
	}
}

func TestReleaseTwiceReturnsNil(t *testing.T) {
	h := newHarness(t)
	sb := h.createUnclosed(t, contracts.NetworkRegistries)
	for i := range 2 {
		if err := h.p.Release(h.ctx, sb.ID()); err != nil {
			t.Fatalf("Release %d: %v", i+1, err)
		}
	}
}

func TestReleaseOfUnknownIDReturnsNil(t *testing.T) {
	h := newHarness(t)
	const unknown = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := h.p.Release(h.ctx, unknown); err != nil {
		t.Fatalf("Release(unknown) = %v, want nil", err)
	}
}

func TestReleaseRefusesEmptyID(t *testing.T) {
	h := newHarness(t)
	if err := h.p.Release(h.ctx, ""); !errors.Is(err, docker.ErrNotSandbox) {
		t.Fatalf("Release(\"\") = %v, want ErrNotSandbox", err)
	}
}

func TestCloseAfterReleaseReturnsNil(t *testing.T) {
	h := newHarness(t)
	sb := h.createUnclosed(t, contracts.NetworkRegistries)
	if err := h.p.Release(h.ctx, sb.ID()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := sb.Close(context.Background()); err != nil {
		t.Fatalf("Close after Release: %v", err)
	}
}

// TestReleaseRefusesUnlabelledContainer is the attack: an ID naming a
// container the provider did not create, such as one a confused or
// tampered attempt row points at, must not be removed.
func TestReleaseRefusesUnlabelledContainer(t *testing.T) {
	h := newHarness(t)
	// A sandbox first, so the image is on the daemon.
	h.create(t, h.spec(t, contracts.NetworkNone))
	created, err := h.cli.ContainerCreate(h.ctx, client.ContainerCreateOptions{
		Name:       "conclave-unlabelled-" + h.Namespace,
		Config:     &container.Config{Image: dockertest.Image(), Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", "sleep 300"}},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		t.Fatalf("creating unlabelled container: %v", err)
	}
	t.Cleanup(func() {
		_, err := h.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			t.Errorf("removing unlabelled container: %v", err)
		}
	})

	err = h.p.Release(h.ctx, created.ID)
	if !errors.Is(err, docker.ErrNotSandbox) {
		t.Fatalf("Release(unlabelled) = %v, want ErrNotSandbox", err)
	}
	if _, err := h.cli.ContainerInspect(h.ctx, created.ID, client.ContainerInspectOptions{}); err != nil {
		t.Fatalf("unlabelled container did not survive Release: %v", err)
	}
}

// TestReleaseLeavesUnlabelledNetwork finds the network by the sandbox's
// name, so a network of that name the provider did not create must be
// left alone while the sandbox itself is released.
func TestReleaseLeavesUnlabelledNetwork(t *testing.T) {
	h := newHarness(t)
	sb := h.createUnclosed(t, contracts.NetworkNone)
	name := strings.TrimPrefix(h.inspect(t, sb.(*docker.Sandbox)).Name, "/")
	net, err := h.cli.NetworkCreate(h.ctx, name, client.NetworkCreateOptions{Driver: "bridge"})
	if err != nil {
		t.Fatalf("creating unlabelled network: %v", err)
	}
	t.Cleanup(func() {
		_, err := h.cli.NetworkRemove(context.Background(), net.ID, client.NetworkRemoveOptions{})
		if err != nil && !cerrdefs.IsNotFound(err) {
			t.Errorf("removing unlabelled network: %v", err)
		}
	})

	if err := h.p.Release(h.ctx, sb.ID()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if left := h.containers(t); len(left) != 0 {
		t.Errorf("containers remain after Release: %v", left)
	}
	if _, err := h.cli.NetworkInspect(h.ctx, net.ID, client.NetworkInspectOptions{}); err != nil {
		t.Fatalf("unlabelled network did not survive Release: %v", err)
	}
}
