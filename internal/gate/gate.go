package gate

import (
	"context"
	"errors"
	"time"

	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
)

// Results persists gate results. store.GateResults implements it using
// only contracts types.
type Results interface {
	// Record inserts every result in one transaction, keeping any row
	// already stored under the same (attempt, gate, artifact hash): the
	// first writer wins (RL-2). It returns the rows as stored.
	Record(ctx context.Context, results []contracts.GateResult) ([]contracts.GateResult, error)

	// ForArtifact returns every result recorded for one attempt and
	// artifact hash, ordered by gate name.
	ForArtifact(ctx context.Context, attemptID, artifactHash string) ([]contracts.GateResult, error)
}

// RejectionCounter counts an attempt's tool calls refused on security
// grounds (FR-SEC29). store.ToolCalls implements it.
type RejectionCounter interface {
	RejectionCount(ctx context.Context, attemptID string) (int, error)
}

// Input is one attempt's artifact and everything needed to gate it.
type Input struct {
	AttemptID string
	Artifact  Artifact

	// TreePath is the prepared tree on the worker the attempt started
	// from. Its HEAD must be Artifact.Base. Staging streams it into the
	// gating sandbox; the caller keeps it until Gate returns.
	TreePath string

	Tier     contracts.SensitivityTier
	Policy   Policy
	Commands Commands

	// InPlace is the agent's sandbox, reset and reused for
	// reset_in_place. Nil forces clean_room. A sandbox handed to Gate is
	// never handed back to an agent.
	InPlace contracts.Sandbox

	// NewSandbox creates a fresh sandbox for clean_room. Gate closes it.
	NewSandbox func(ctx context.Context) (contracts.Sandbox, error)

	ForceCleanRoom bool
}

// Outcome is the verdict on one artifact.
type Outcome struct {
	Passed  bool
	Flagged bool
	Mode    contracts.GatingMode
	Results []contracts.GateResult
}

// Gater runs the gates and records their results.
type Gater struct {
	Blobs      blob.Store
	Results    Results
	Rejections RejectionCounter
	Secrets    SecretScanner

	// Now stamps results; nil means time.Now.
	Now func() time.Time
}

// verdict is one gate's outcome before it becomes a GateResult.
type verdict struct {
	Gate    contracts.GateName
	Passed  bool
	Flagged bool

	// Detail is a short summary written by the platform. Sandbox output
	// never goes here; it goes to the blob store under Key.
	Detail   string
	Key      string
	Duration time.Duration
}

// RequiredGates are the gates every artifact must have a passing result
// for before it may be pushed. Flag-only gates are included: they pass
// and carry the flag.
var RequiredGates = []contracts.GateName{
	contracts.GateDiffSize,
	contracts.GateProtected,
	contracts.GateWeakening,
	contracts.GateExecSurface,
	contracts.GateToolCalls,
	contracts.GateSecrets,
	contracts.GateBuild,
	contracts.GateLint,
	contracts.GateTests,
}

// Gate runs every gate against in.Artifact, or returns the results
// already recorded for it.
func (g *Gater) Gate(ctx context.Context, in Input) (Outcome, error) {
	return Outcome{}, errors.New("not implemented")
}

// newResult is the only constructor of a GateResult, so every result
// carries the hash of the artifact it checked.
func newResult(attemptID string, art Artifact, mode contracts.GatingMode, v verdict, now time.Time) contracts.GateResult {
	return contracts.GateResult{
		AttemptID:    attemptID,
		Gate:         v.Gate,
		Passed:       v.Passed,
		Flagged:      v.Flagged,
		ArtifactHash: art.Hash,
		Mode:         mode,
		Detail:       v.Detail,
		ArtifactKey:  v.Key,
		RanAt:        now,
		Duration:     v.Duration,
	}
}
