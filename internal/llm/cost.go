package llm

import (
	"cmp"
	"context"
	"crypto/rand"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// CostLedger is the boundary to recorded and reserved spend. It enforces
// the FR-SEC9 caps across every scope and every process: each model call
// reserves its claim before it is sent and settles it after, and the
// reservation is what a concurrent scope sees while the call is in flight.
type CostLedger interface {
	// Attributed reports whether the feature request belongs to the
	// repository and, when attemptID is not empty, the attempt to the
	// feature request. A scope is refused otherwise, because spend charged
	// to the wrong feature request would dodge its cap.
	Attributed(ctx context.Context, repoID, featureRequestID, attemptID string) (bool, error)

	// Reserve calls decide with the spend settled and reserved so far for
	// the attempt (none when attemptID is empty), the feature request, and
	// the current UTC month, and stores the claim it returns as a
	// reservation that expires after ttl unless settled first. Every
	// Reserve is serialized with every other, so two calls cannot both
	// pass a cap on the same headroom. An error from decide is returned
	// unchanged and nothing is reserved. decide must not block.
	Reserve(ctx context.Context, attemptID, featureRequestID string, ttl time.Duration, decide func(Spend) (Claim, error)) (string, error)

	// Settle records ev and deletes the reservation atomically. A second
	// event with the same IdempotencyKey must be ignored, not rejected
	// (RL-2), so a retried Settle counts the spend once and succeeds.
	Settle(ctx context.Context, reservationID string, ev contracts.CostEvent) error

	// Release deletes a reservation whose call billed nothing.
	Release(ctx context.Context, reservationID string) error
}

// costUSD prices a call from the model's configured per-million-token rates.
func costUSD(m ModelConfig, r ProviderResponse) float64 {
	write := cmp.Or(m.CacheWriteUSDPerMTok, m.InputUSDPerMTok)
	read := cmp.Or(m.CacheReadUSDPerMTok, m.InputUSDPerMTok)
	usd := float64(r.InputTokens)*m.InputUSDPerMTok +
		float64(r.CacheWriteTokens)*write +
		float64(r.CacheReadTokens)*read +
		float64(r.OutputTokens)*m.OutputUSDPerMTok
	return usd / 1e6
}

// billable is the usage a reply is charged for, worked out once where the
// router receives it so every place that charges it agrees.
//
// A negative count, which no honest provider reports, counts as zero, so it
// can never credit the budget whatever an adapter lets through. Each field
// is clamped on its own, so a negative count cannot cancel a real one.
//
// A successful reply that reports no usage at all is charged input and
// output, the input estimate and output limit the call was sent with, which
// is the most it can have cost. An embedding is charged its estimate the
// same way. Charging nothing would let a provider that never reports usage
// be called without limit. A failed reply with no usage is left at zero:
// the router does not record those, because the request may never have
// reached the provider.
func billable(r ProviderResponse, succeeded bool, input, output int) ProviderResponse {
	r.InputTokens = max(r.InputTokens, 0)
	r.CacheWriteTokens = max(r.CacheWriteTokens, 0)
	r.CacheReadTokens = max(r.CacheReadTokens, 0)
	r.OutputTokens = max(r.OutputTokens, 0)
	if succeeded && r.totalTokens() == 0 {
		r.InputTokens, r.OutputTokens = input, output
	}
	return r
}

// usageOf is what a reply is charged as. Cache traffic counts as input.
func usageOf(m ModelConfig, r ProviderResponse) contracts.Usage {
	return contracts.Usage{
		InputTokens:  r.InputTokens + r.CacheWriteTokens + r.CacheReadTokens,
		OutputTokens: r.OutputTokens,
		CostUSD:      costUSD(m, r),
	}
}

// costKey identifies one call's cost event. It is keyed on the provider's
// reply ID, not on the call's position in a session: a retried activity
// makes new calls with new replies, and their spend is real and must be
// recorded, while recording the same reply twice must not count twice.
// Without a reply ID the key is random, which records once and never
// deduplicates, the safe direction for a ledger that enforces caps.
// The path is live: an adapter that refuses a reply for having no ID still
// returns the usage the provider billed, with ErrMalformedReply, and the
// router records that usage here. Embedding replies need not carry an ID
// either. A retried write of such an event is counted twice.
func costKey(prefix, provider, replyID string) string {
	if replyID == "" {
		replyID = "local-" + rand.Text()
	}
	return prefix + "/" + provider + "/" + replyID
}
