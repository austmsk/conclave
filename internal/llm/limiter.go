package llm

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Default rate limits, used when configuration leaves them unset. They are
// sized for Milestone 1, where one implementer loop makes a call every few
// seconds: 20 requests a minute leaves room for about three loops at once,
// and 300,000 tokens a minute is roughly six mid-session calls. They bound
// how fast money can be spent; budgets bound how much.
const (
	DefaultRequestsPerMinute = 20
	DefaultTokensPerMinute   = 300_000
)

// limiter is a pair of token buckets, one for requests and one for model
// tokens, shared by every scope of a router. Callers reserve their estimate
// before a call and settle the difference after, so concurrent calls cannot
// all pass on the same headroom. The tokens bucket may go negative when a
// call uses more than it reserved; later callers wait for it to refill.
type limiter struct {
	mu sync.Mutex

	reqRate, tokRate float64 // per second
	reqCap, tokCap   float64
	reqs, toks       float64
	last             time.Time

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func newLimiter(rl RateLimit) *limiter {
	rpm, tpm := float64(rl.RequestsPerMinute), float64(rl.TokensPerMinute)
	return &limiter{
		reqRate: rpm / 60, tokRate: tpm / 60,
		reqCap: rpm, tokCap: tpm,
		reqs: rpm, toks: tpm,
		last:  time.Now(),
		now:   time.Now,
		sleep: sleepCtx,
	}
}

// acquire waits until one request and tokens are available, then takes
// them. A reservation larger than the bucket waits for a full bucket rather
// than forever.
func (l *limiter) acquire(ctx context.Context, tokens int) error {
	for {
		wait := l.tryAcquire(float64(tokens))
		if wait == 0 {
			return nil
		}
		if err := l.sleep(ctx, wait); err != nil {
			return fmt.Errorf("waiting for rate limit: %w", err)
		}
	}
}

// tryAcquire takes capacity and returns zero, or returns how long to wait.
func (l *limiter) tryAcquire(tokens float64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	need := min(tokens, l.tokCap)
	if l.reqs >= 1 && l.toks >= need {
		l.reqs--
		l.toks -= tokens
		return 0
	}
	wait := max((1-l.reqs)/l.reqRate, (need-l.toks)/l.tokRate)
	return max(time.Duration(wait*float64(time.Second)), time.Millisecond)
}

// settle returns a reservation and charges what the call actually used.
func (l *limiter) settle(reserved, used int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.toks = min(l.toks+float64(reserved-used), l.tokCap)
}

func (l *limiter) refill() {
	now := l.now()
	elapsed := now.Sub(l.last).Seconds()
	l.last = now
	if elapsed <= 0 {
		return
	}
	l.reqs = min(l.reqs+elapsed*l.reqRate, l.reqCap)
	l.toks = min(l.toks+elapsed*l.tokRate, l.tokCap)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
