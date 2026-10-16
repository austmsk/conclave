package faketest

import (
	"context"
	"sync"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
)

// Costs is an in-memory llm.CostLedger with the same idempotency rule as
// the cost_events table: a second event with a recorded key is ignored.
// The Postgres ledger's own test checks that the table behaves this way.
type Costs struct {
	mu     sync.Mutex
	events []contracts.CostEvent
	keys   map[string]bool

	// Err, when set, is returned by every RecordCost and AttemptTokens
	// call.
	Err error
}

var _ llm.CostLedger = (*Costs)(nil)

// RecordCost stores ev unless its key has been seen.
func (c *Costs) RecordCost(_ context.Context, ev contracts.CostEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return c.Err
	}
	if c.keys == nil {
		c.keys = map[string]bool{}
	}
	if c.keys[ev.IdempotencyKey] {
		return nil
	}
	c.keys[ev.IdempotencyKey] = true
	c.events = append(c.events, ev)
	return nil
}

// Events returns a copy of the recorded events, in order.
func (c *Costs) Events() []contracts.CostEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]contracts.CostEvent(nil), c.events...)
}

// AttemptTokens sums input and output tokens over the attempt's events.
func (c *Costs) AttemptTokens(_ context.Context, attemptID string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return 0, c.Err
	}
	total := 0
	for _, ev := range c.events {
		if ev.AttemptID == attemptID {
			total += ev.InputTokens + ev.OutputTokens
		}
	}
	return total, nil
}
