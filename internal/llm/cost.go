package llm

import (
	"context"

	"github.com/austmsk/conclave/internal/contracts"
)

// CostRecorder persists one cost event per billable model call. It is the
// boundary to the cost_events table. Implementations must ignore a second
// event with the same IdempotencyKey (RL-2): a retried activity replays the
// same keys and must not count its spend twice.
type CostRecorder interface {
	RecordCost(ctx context.Context, ev contracts.CostEvent) error
}

// costUSD prices a call from the model's configured per-million-token rates.
func costUSD(m ModelConfig, inputTokens, outputTokens int) float64 {
	return (float64(inputTokens)*m.InputUSDPerMTok + float64(outputTokens)*m.OutputUSDPerMTok) / 1e6
}
