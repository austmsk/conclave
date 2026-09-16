package contracts

import "time"

// AttemptState is the lifecycle of one implementation attempt.
type AttemptState string

const (
	AttemptRunning AttemptState = "running"
	AttemptPassed  AttemptState = "passed"
	AttemptFailed  AttemptState = "failed"

	// AttemptErrored is infrastructure failure, not agent failure. Keeping
	// these separate matters: counting platform errors as agent failures
	// silently depresses every resolve rate measured in Milestone 2.
	AttemptErrored AttemptState = "errored"
)

// FailureReason explains a failed or errored attempt.
type FailureReason string

const (
	ReasonGateFailed    FailureReason = "gate_failed"
	ReasonStepLimit     FailureReason = "step_limit"
	ReasonTokenLimit    FailureReason = "token_limit"
	ReasonWallClock     FailureReason = "wall_clock"
	ReasonRepeatErrors  FailureReason = "repeat_errors"
	ReasonInvalidOutput FailureReason = "invalid_output"

	ReasonSandboxError  FailureReason = "sandbox_error"
	ReasonProviderError FailureReason = "provider_error"
)

// Attempt is the durable record of one try at one task.
type Attempt struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`

	// Ordinal distinguishes competing attempts at the same task.
	Ordinal int `json:"ordinal"`

	State  AttemptState  `json:"state"`
	Reason FailureReason `json:"reason,omitempty"`

	Provenance Provenance `json:"provenance"`
	Usage      Usage      `json:"usage"`

	// ArtifactHash identifies the diff this attempt produced. The push path
	// refuses any artifact whose recomputed hash does not match a passing
	// gate result (FR-SEC27).
	ArtifactHash string `json:"artifact_hash,omitempty"`

	Report *CompletionReport `json:"report,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	// SandboxReleasedAt is tracked separately from State because cleanup must
	// happen however the attempt ended — including cancellation. The orphan
	// reaper looks for attempts in a final state with this still unset (RL-3).
	SandboxReleasedAt time.Time `json:"sandbox_released_at,omitempty"`
}
