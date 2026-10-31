package activity

import (
	"context"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
)

// startHeartbeat calls beat every interval from its own goroutine until
// the returned stop is called or ctx ends. The attempt activity spends
// most of its time inside the runner and the gate, neither of which knows
// about Temporal, and the router's rate limiter blocks with no hook, so
// the heartbeat cannot come from the work itself. Cancellation reaches
// the work through ctx, which the runner honours; Temporal delivers a
// cancellation to an activity only through a heartbeat. stop waits for
// the goroutine to exit, so no beat happens after the activity returns.
func startHeartbeat(ctx context.Context, interval time.Duration, beat func(context.Context)) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				beat(ctx)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}

// recordHeartbeat is the production beat. It carries no details: a
// retried attempt does not resume, so there is no progress to hand on.
func recordHeartbeat(ctx context.Context) {
	activity.RecordHeartbeat(ctx)
}
