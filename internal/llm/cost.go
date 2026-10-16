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
// Adapters reject a reply with no ID, so this is the path only for
// providers that never issue one.
func costKey(prefix, provider, replyID string) string {
	if replyID == "" {
		replyID = "local-" + rand.Text()
	}
	return prefix + "/" + provider + "/" + replyID
}
