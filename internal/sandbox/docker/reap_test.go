package docker_test

import (
	"context"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

func TestReapRemovesOrphansAndSparesTheLiving(t *testing.T) {
	h := newHarness(t)

	// An orphan: created and abandoned without Close, as a crashed worker
	// would leave it. A registries sandbox, so its network is orphaned too.
	orphan, err := h.p.Create(h.ctx, h.spec(t, contracts.NetworkRegistries))
	if err != nil {
		t.Fatal(err)
	}
	orphanID := orphan.ID()
	living := h.create(t, h.spec(t, contracts.NetworkNone))

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
	// A reaped sandbox's Close still succeeds: the container is simply gone.
	if err := living.Close(context.Background()); err != nil {
		t.Errorf("Close after Reap: %v", err)
	}
	if err := orphan.Close(context.Background()); err != nil {
		t.Errorf("orphan Close after Reap: %v", err)
	}
}

func TestReapIgnoresOtherNamespaces(t *testing.T) {
	a := newHarness(t)
	b := newHarness(t)
	a.create(t, a.spec(t, contracts.NetworkNone))

	if n, err := b.p.Reap(b.ctx, 0); err != nil || n != 0 {
		t.Fatalf("Reap in another namespace = %d, %v; want 0", n, err)
	}
	if len(a.containers(t)) != 1 {
		t.Error("a sandbox in another namespace was reaped")
	}
}
