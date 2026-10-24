package gate

import (
	"fmt"

	"github.com/austmsk/conclave/internal/contracts"
)

// SelectMode chooses the gating mode. It is the only place the default
// lives, so Milestone 2 can set it from data.
//
// clean_room is chosen when there is no agent sandbox to reuse, when the
// caller forces it, when the policy says so, for the sensitive and
// local_only tiers, and when any static gate flagged. Otherwise the
// agent's sandbox is reset and reused. An empty or unknown tier, or an
// unknown policy mode, is an error rather than a guess.
func SelectMode(tier contracts.SensitivityTier, p Policy, haveInPlace, force, flagged bool) (contracts.GatingMode, error) {
	strictTier := false
	switch tier {
	case contracts.TierPublic, contracts.TierPrivate:
	case contracts.TierSensitive, contracts.TierLocalOnly:
		strictTier = true
	default:
		return "", fmt.Errorf("selecting gating mode: unknown sensitivity tier %q", tier)
	}
	switch p.Mode {
	case "", contracts.GatingResetInPlace, contracts.GatingCleanRoom:
	default:
		return "", fmt.Errorf("selecting gating mode: unknown policy mode %q", p.Mode)
	}
	if !haveInPlace || force || strictTier || flagged || p.Mode == contracts.GatingCleanRoom {
		return contracts.GatingCleanRoom, nil
	}
	return contracts.GatingResetInPlace, nil
}
