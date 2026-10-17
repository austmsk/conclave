package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeClock drives a limiter without real sleeps: sleeping advances it.
type fakeClock struct {
	t     time.Time
	slept []time.Duration
}

func testLimiter(rl RateLimit) (*limiter, *fakeClock) {
	c := &fakeClock{t: time.Unix(0, 0)}
	l := newLimiter(rl)
	l.last = c.t
	l.now = func() time.Time { return c.t }
	l.sleep = func(_ context.Context, d time.Duration) error {
		c.slept = append(c.slept, d)
		c.t = c.t.Add(d)
		return nil
	}
	return l, c
}

func (c *fakeClock) total() time.Duration {
	var sum time.Duration
	for _, d := range c.slept {
		sum += d
	}
	return sum
}

func TestLimiterRequests(t *testing.T) {
	l, c := testLimiter(RateLimit{RequestsPerMinute: 2, TokensPerMinute: 1_000_000})
	ctx := context.Background()
	for range 2 {
		if err := l.acquire(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	if c.total() != 0 {
		t.Fatalf("waited %v within the burst", c.total())
	}
	if err := l.acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := c.total(); got < 29*time.Second || got > 31*time.Second {
		t.Fatalf("third request waited %v, want about 30s", got)
	}
}

// TestLimiterChargesActualUse: a call that used more than it reserved
// drives the bucket negative, and the next caller waits for the overage.
func TestLimiterChargesActualUse(t *testing.T) {
	l, c := testLimiter(RateLimit{RequestsPerMinute: 100, TokensPerMinute: 60_000})
	ctx := context.Background()
	if err := l.acquire(ctx, 1_000); err != nil {
		t.Fatal(err)
	}
	l.settle(1_000, 90_000) // 30,000 tokens over the bucket.
	if err := l.acquire(ctx, 1_000); err != nil {
		t.Fatal(err)
	}
	// 31,000 tokens at 1,000 a second.
	if got := c.total(); got < 30*time.Second || got > 32*time.Second {
		t.Fatalf("waited %v, want about 31s", got)
	}
}

// TestLimiterRefundsUnusedReservation: settling under the reservation
// returns the difference, so the next call does not wait.
func TestLimiterRefundsUnusedReservation(t *testing.T) {
	l, c := testLimiter(RateLimit{RequestsPerMinute: 100, TokensPerMinute: 10_000})
	ctx := context.Background()
	if err := l.acquire(ctx, 9_000); err != nil {
		t.Fatal(err)
	}
	l.settle(9_000, 100)
	if err := l.acquire(ctx, 9_000); err != nil {
		t.Fatal(err)
	}
	if c.total() != 0 {
		t.Fatalf("waited %v after a refund", c.total())
	}
}

// TestLimiterOversizedReservation: a call larger than the whole bucket
// waits for a full bucket instead of forever.
func TestLimiterOversizedReservation(t *testing.T) {
	l, c := testLimiter(RateLimit{RequestsPerMinute: 100, TokensPerMinute: 1_000})
	ctx := context.Background()
	if err := l.acquire(ctx, 5_000); err != nil {
		t.Fatal(err)
	}
	if c.total() != 0 {
		t.Fatalf("waited %v with a full bucket", c.total())
	}
	if err := l.acquire(ctx, 5_000); err != nil {
		t.Fatal(err)
	}
	// 4,000 overdrawn plus a full 1,000-token bucket, at about 16.7 a second.
	if got := c.total(); got < 4*time.Minute || got > 6*time.Minute {
		t.Fatalf("waited %v, want about 5m", got)
	}
}

func TestLimiterStopsOnContext(t *testing.T) {
	l := newLimiter(RateLimit{RequestsPerMinute: 1, TokensPerMinute: 1_000})
	ctx, cancel := context.WithCancel(context.Background())
	if err := l.acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := l.acquire(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want Canceled", err)
	}
}
