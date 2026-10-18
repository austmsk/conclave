package faketest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
)

// Costs is an in-memory llm.CostLedger with the same rules as the store's:
// a second event with a recorded key is ignored, every Reserve is
// serialized, a live reservation counts against the caps until it is
// settled or released, and a cancelled context fails Settle and Release as
// it would fail a database call. It has no clock, so reservations never expire and
// every event counts toward the month; the store's tests cover both.
type Costs struct {
	mu     sync.Mutex
	events []contracts.CostEvent
	keys   map[string]bool
	holds  map[string]hold
	nextID int

	// Known lists the attributions Attributed accepts. Anything else is
	// refused, as the store refuses IDs that are not linked in the schema.
	Known []Attribution

	// Err, when set, is returned by every method.
	Err error

	// SettleErr, when set, is returned by Settle and Release, which then
	// leave the reservation in place, as a failed store transaction does.
	SettleErr error
}

// Attribution links an attempt to its feature request and repository. An
// empty AttemptID makes the entry a feature request with no attempt.
type Attribution struct {
	RepoID, FeatureRequestID, AttemptID string
}

type hold struct {
	attemptID, featureRequestID string
	claim                       llm.Claim
}

// ErrUnknownReservation is returned by Settle and Release for an ID Reserve
// never issued, which the store would silently ignore. The fake is stricter
// so a router that settles the wrong reservation fails its test.
var ErrUnknownReservation = errors.New("no such reservation")

var _ llm.CostLedger = (*Costs)(nil)

// Attributed accepts an attempt listed in Known, and a feature request with
// no attempt when any entry names it and its repository.
func (c *Costs) Attributed(_ context.Context, repoID, featureRequestID, attemptID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return false, c.Err
	}
	return slices.ContainsFunc(c.Known, func(a Attribution) bool {
		return a.RepoID == repoID && a.FeatureRequestID == featureRequestID &&
			(attemptID == "" || a.AttemptID == attemptID)
	}), nil
}

// Reserve sums recorded and reserved spend and stores decide's claim.
func (c *Costs) Reserve(_ context.Context, attemptID, featureRequestID string, _ time.Duration, decide func(llm.Spend) (llm.Claim, error)) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return "", c.Err
	}
	claim, err := decide(c.spend(attemptID, featureRequestID))
	if err != nil {
		return "", err
	}
	if c.holds == nil {
		c.holds = map[string]hold{}
	}
	c.nextID++
	id := fmt.Sprintf("reservation-%d", c.nextID)
	c.holds[id] = hold{attemptID: attemptID, featureRequestID: featureRequestID, claim: claim}
	return id, nil
}

func (c *Costs) spend(attemptID, featureRequestID string) llm.Spend {
	var sp llm.Spend
	add := func(attempt, feature string, tokens int, usd float64) {
		if attemptID != "" && attempt == attemptID {
			sp.AttemptTokens += tokens
		}
		if feature == featureRequestID {
			sp.FeatureRequestTokens += tokens
		}
		sp.MonthUSD += usd
	}
	for _, ev := range c.events {
		add(ev.AttemptID, ev.FeatureRequestID, ev.InputTokens+ev.OutputTokens, ev.CostUSD)
	}
	for _, h := range c.holds {
		add(h.attemptID, h.featureRequestID, h.claim.Tokens, h.claim.CostUSD)
	}
	return sp
}

// Settle stores ev unless its key has been seen, and drops the reservation.
// Settling a reservation already settled is allowed, as a retried write.
func (c *Costs) Settle(ctx context.Context, reservationID string, ev contracts.CostEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := cmp.Or(c.Err, c.SettleErr, ctx.Err()); err != nil {
		return err
	}
	if c.keys == nil {
		c.keys = map[string]bool{}
	}
	if c.keys[ev.IdempotencyKey] {
		delete(c.holds, reservationID)
		return nil
	}
	if err := c.drop(reservationID); err != nil {
		return err
	}
	c.keys[ev.IdempotencyKey] = true
	c.events = append(c.events, ev)
	return nil
}

// Release drops a reservation.
func (c *Costs) Release(ctx context.Context, reservationID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := cmp.Or(c.Err, c.SettleErr, ctx.Err()); err != nil {
		return err
	}
	return c.drop(reservationID)
}

func (c *Costs) drop(reservationID string) error {
	if _, ok := c.holds[reservationID]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownReservation, reservationID)
	}
	delete(c.holds, reservationID)
	return nil
}

// Events returns a copy of the recorded events, in order.
func (c *Costs) Events() []contracts.CostEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]contracts.CostEvent(nil), c.events...)
}

// Reservations reports how many reservations are live.
func (c *Costs) Reservations() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.holds)
}

// AttemptTokens sums input and output tokens over the attempt's events.
func (c *Costs) AttemptTokens(attemptID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, ev := range c.events {
		if ev.AttemptID == attemptID {
			total += ev.InputTokens + ev.OutputTokens
		}
	}
	return total
}
