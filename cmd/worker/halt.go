package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/store"
)

// The kill switch, as the worker sees it (FR-SEC11). `conclave halt`
// records a halt in Postgres and cancels every open workflow; the worker
// polls for it, keeps running for the grace period so the cancellations
// it receives can release their sandboxes, then stops. It refuses to start
// while the halt stands.
const (
	haltPoll = 5 * time.Second
	// haltGrace is the default wait between seeing a halt and stopping.
	// It exceeds the attempt heartbeat timeout, so a cancelled attempt
	// hears of its cancellation on a heartbeat and reports before the
	// worker goes.
	haltGrace = 3 * time.Minute
)

// errHalted is how the worker exits on a halt.
var errHalted = errors.New("halted by the kill switch; run conclave resume to start work again")

// haltState reads the kill switch's state: store.CurrentHalt in
// production.
type haltState func(context.Context) (store.HaltState, error)

// refuseIfHalted stops startup while a halt stands.
func refuseIfHalted(ctx context.Context, state haltState) error {
	st, err := state(ctx)
	if err != nil {
		return fmt.Errorf("reading the kill switch: %w", err)
	}
	if st.Halted {
		// The CLI redacts the reason before storing it; this redacts it
		// again, since the error is printed as plain text.
		return fmt.Errorf("halt %d since %s (%s): %w", st.ID, st.Since.UTC().Format(time.RFC3339), github.RedactText(st.Reason), errHalted)
	}
	return nil
}

// checkHaltGrace refuses a grace period that does not exceed the attempt
// heartbeat timeout: a worker that stops sooner can go before a cancelled
// attempt hears of its cancellation and releases its sandbox, leaving
// the halt's sweep as the only cleanup (NFR-R3).
func checkHaltGrace(grace time.Duration) error {
	if grace <= activity.HeartbeatTimeout {
		return fmt.Errorf("-halt-grace %s must exceed the attempt heartbeat timeout, %s, or cancelled attempts may not release their sandboxes", grace, activity.HeartbeatTimeout)
	}
	return nil
}

// watchHalt polls state every poll and, once it sees a halt, waits grace
// and calls stop. It returns when it has stopped the worker or ctx ends.
// A failed read is logged and polled again: a database outage does not
// stop the worker, and a worker that cannot reach Postgres cannot do work
// anyway.
func watchHalt(ctx context.Context, state haltState, poll, grace time.Duration, stop func(), log *slog.Logger) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := state(ctx)
		if err != nil {
			log.Error("reading the kill switch", "error", err)
			continue
		}
		if !st.Halted {
			continue
		}
		log.Warn("kill switch set; stopping after the grace period", "halt_id", st.ID, "grace", grace.String())
		select {
		case <-ctx.Done():
		case <-time.After(grace):
		}
		stop()
		return
	}
}
