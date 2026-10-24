package gate

import (
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

// SelectMode chooses the gating mode. It is the only place the default
// lives, so Milestone 2 can set it from data.
func SelectMode(tier contracts.SensitivityTier, p Policy, haveInPlace, force, flagged bool) (contracts.GatingMode, error) {
	return "", errors.New("not implemented")
}
