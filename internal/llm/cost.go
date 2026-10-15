package llm

import (
	"cmp"
	"context"
	"crypto/rand"

	"github.com/austmsk/conclave/internal/contracts"
)

// CostRecorder persists one cost event per billable model call. It is the
// boundary to the cost_events table. Implementations must ignore a second
// event with the same IdempotencyKey (RL-2), so a reply recorded twice, for
// example by a retried write, is counted once.
type CostRecorder interface {
	RecordCost(ctx context.Context, ev contracts.CostEvent) error
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

// costKey identifies one call's cost event. It is keyed on the provider's
// reply ID, not on the call's position in a session: a retried activity
// makes new calls with new replies, and their spend is real and must be
// recorded, while recording the same reply twice must not count twice.
// Without a reply ID the key is random, which records once and never
// deduplicates, the safe direction for a ledger that enforces caps.
func costKey(prefix, provider, replyID string) string {
	if replyID == "" {
		replyID = "local-" + rand.Text()
	}
	return prefix + "/" + provider + "/" + replyID
}
