package llm

import (
	"cmp"
	"context"
	"crypto/rand"

	"github.com/austmsk/conclave/internal/contracts"
)

// CostLedger is the boundary to the cost_events table: it records one event
// per billable model call and reports what an attempt has already spent.
type CostLedger interface {
	// RecordCost persists ev. A second event with the same IdempotencyKey
	// must be ignored, not rejected (RL-2), so a reply recorded twice, for
	// example by a retried write, is counted once and the retry succeeds.
	RecordCost(ctx context.Context, ev contracts.CostEvent) error

	// AttemptTokens is the input plus output tokens recorded for an
	// attempt. A scope seeds its budget from it, so a retried activity does
	// not get a fresh budget.
	AttemptTokens(ctx context.Context, attemptID string) (int, error)
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
