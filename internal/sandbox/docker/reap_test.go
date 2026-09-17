package docker_test

import (
	"context"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox/docker"
)

func TestReapRemovesOrphansOlderThanTheGivenAge(t *testing.T) {
	h := newHarness(t)

	// An orphan: created and abandoned without Close, as a crashed worker
	// would leave it. Built through the install path so its network is
	// orphaned too.
	orphan, _, err := h.p.CreateWithInstall(h.ctx, h.spec(t, contracts.NetworkRegistries), nil)
	if err != nil {
		t.Fatal(err)
	}
	orphanID := orphan.ID()
	second := h.create(t, h.spec(t, contracts.NetworkNone))

	if n, err := h.p.Reap(h.ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("Reap(1h) = %d, %v; want 0 and no error for young sandboxes", n, err)
	}
	if len(h.containers(t)) != 2 {
		t.Fatal("a young sandbox was reaped")
	}

	n, err := h.p.Reap(h.ctx, 0)
	if err != nil {
		t.Fatalf("Reap(0): %v", err)
	}
	if n != 2 {
		t.Errorf("Reap(0) removed %d containers, want 2", n)
	}
	if left := h.containers(t); len(left) != 0 {
		t.Errorf("containers remain after Reap: %v", left)
	}
	if h.networks(t) != 0 {
		t.Error("orphaned network remains after Reap")
	}
	for _, c := range h.containers(t) {
		if c.ID == orphanID {
			t.Error("orphan survived")
		}
	}
	// A second sweep finds nothing and counts nothing.
	if n, err := h.p.Reap(h.ctx, 0); err != nil || n != 0 {
		t.Errorf("second Reap(0) = %d, %v; want 0", n, err)
	}
	// A reaped sandbox's Close still succeeds: the container is simply gone.
	if err := second.Close(context.Background()); err != nil {
		t.Errorf("Close after Reap: %v", err)
	}
	if err := orphan.Close(context.Background()); err != nil {
		t.Errorf("orphan Close after Reap: %v", err)
	}
}

func TestReapSweepsOtherNamespacesOnlyPastStaleAge(t *testing.T) {
	a := newHarness(t)
	b := newHarness(t)
	a.create(t, a.spec(t, contracts.NetworkNone))

	if n, err := b.p.Reap(b.ctx, 0); err != nil || n != 0 {
		t.Fatalf("Reap in another namespace = %d, %v; want 0 under the default stale age", n, err)
	}
	if len(a.containers(t)) != 1 {
		t.Fatal("a fresh sandbox in another namespace was reaped")
	}

	// A provider whose stale age has passed removes it regardless of
	// namespace, so a renamed namespace cannot strand containers.
	stale, err := docker.New(b.ctx, docker.Config{Namespace: b.namespace, StaleAge: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stale.Close() }()
	time.Sleep(1100 * time.Millisecond) // container Created has one-second resolution
	if n, err := stale.Reap(b.ctx, 0); err != nil || n != 1 {
		t.Fatalf("stale Reap = %d, %v; want 1", n, err)
	}
	if len(a.containers(t)) != 0 {
		t.Error("stale sandbox in another namespace survived")
	}
}
