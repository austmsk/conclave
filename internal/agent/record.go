package agent

import (
	"context"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// ToolCallRecord is one row of the tool-call audit (FR-SEC24). The worker
// writes it before the result returns to the model, so an agent cannot
// alter its own trail. Every field is set by the runner; nothing in it is
// read back from the sandbox.
type ToolCallRecord struct {
	AttemptID  string
	SessionKey string

	// Step numbers the session's tool calls from 1, contiguously, in the
	// order they were answered. Calls refused or not executed take a step.
	Step int

	CallID string
	Tool   contracts.ToolName

	// ArgumentsRaw is exactly what the model sent, even when it is not
	// valid JSON.
	ArgumentsRaw []byte

	ExitCode int
	Duration time.Duration

	// ArtifactKey points at the full output in the blob store. It is set
	// whenever the model was shown a truncated result.
	ArtifactKey string

	// Error is sanitized: a short code naming what refused or failed,
	// never raw provider or sandbox output, which may carry a credential.
	Error string

	// Rejected marks a path or argument the tool refused on security
	// grounds (FR-SEC29). Gating counts these per attempt.
	Rejected  bool
	Truncated bool

	// OccurredAt is stamped by the worker when the call finished.
	OccurredAt time.Time
}

// Recorder persists tool-call records. store.ToolCalls implements it.
// Record must be idempotent: a second call with an identical record is
// absorbed, and one that differs from what is stored under the same
// (AttemptID, SessionKey, Step) is an error.
type Recorder interface {
	Record(ctx context.Context, rec ToolCallRecord) error
}
