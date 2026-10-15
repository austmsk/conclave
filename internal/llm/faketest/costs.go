package faketest

import (
	"context"
	"sync"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
)

// Costs is an in-memory llm.CostRecorder with the same idempotency rule as
// the cost_events table: a second event with a recorded key is ignored.
type Costs struct {
	mu     sync.Mutex
	events []contracts.CostEvent
	keys   map[string]bool

	// Err, when set, is returned by every RecordCost call.
	Err error
}

var _ llm.CostRecorder = (*Costs)(nil)

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
