package agent

import (
	"context"

	"github.com/austmsk/conclave/internal/contracts"
)

// ToolCallRecord is one row of the tool-call audit (FR-SEC24). It lives in
// contracts so the gate and the store can read it without importing agent.
type ToolCallRecord = contracts.ToolCallRecord

// Recorder persists tool-call records. store.ToolCalls implements it.
// Record must be idempotent: a second call with an identical record is
// absorbed, and one that differs from what is stored under the same
// (AttemptID, SessionKey, Step) is an error.
type Recorder interface {
	Record(ctx context.Context, rec ToolCallRecord) error
}
