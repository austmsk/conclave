package contracts

import "time"

// GateName identifies one deterministic check. Gates are pass/fail with no
// model involved, and they run before any judging: passing proves an attempt
// works, which is a prerequisite for asking which working solution is best.
type GateName string

const (
	GateBuild     GateName = "build"
	GateLint      GateName = "lint"
	GateTests     GateName = "tests"
	GateNewTests  GateName = "new_tests"
	GateDiffSize  GateName = "diff_size"
	GateSecrets   GateName = "secrets"
	GateDeps      GateName = "dependencies"
	GateProtected GateName = "protected_paths"

	// GateWeakening rejects attempts that delete or skip tests or alter test
	// configuration — the most likely way an agent makes its own gate pass
	// without doing the work (FR-A3).
	GateWeakening GateName = "gate_weakening"

	// GateExecSurface flags changes to files that run code at install, build,
	// or commit time, or that reconfigure the owner's AI tooling (FR-SEC25).
	GateExecSurface GateName = "execution_surface"
)

// GatingMode selects how the recorded artifact is gated.
type GatingMode string

const (
	// GatingCleanRoom rebuilds a fresh sandbox from the base commit plus the
	// diff alone, so no agent-modified environment carries over. Used for
	// sensitive repositories and flagged attempts.
	GatingCleanRoom GatingMode = "clean_room"

	// GatingResetInPlace resets the agent's sandbox to base and reapplies the
	// recorded diff. Preserves the artifact guarantee but not the environment
	// guarantee; cheaper because dependencies stay warm.
	GatingResetInPlace GatingMode = "reset_in_place"
)

// GateResult is the outcome of one gate against one artifact.
type GateResult struct {
	ID        string   `json:"id"`
	AttemptID string   `json:"attempt_id"`
	Gate      GateName `json:"gate"`

	Passed bool `json:"passed"`

	// Flagged marks a gate that did not fail but requires human attention,
	// such as an execution-surface change.
	Flagged bool `json:"flagged"`

	// ArtifactHash ties this result to the exact diff that was checked.
	ArtifactHash string `json:"artifact_hash"`

	Mode GatingMode `json:"mode"`

	// Detail is a short human-readable summary; full output lives at
	// ArtifactKey in the blob store.
	Detail      string `json:"detail,omitempty"`
	ArtifactKey string `json:"artifact_key,omitempty"`

	RanAt    time.Time     `json:"ran_at"`
	Duration time.Duration `json:"duration"`
}
