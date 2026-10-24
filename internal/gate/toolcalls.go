package gate

import (
	"fmt"

	"github.com/austmsk/conclave/internal/contracts"
)

// checkToolCalls flags, and never fails, an attempt with at least
// FlagRejectionsAt tool calls refused on security grounds (FR-SEC29).
// Repeated probing of the path rules is a sign of injected instructions;
// the flag moves gating to clean_room.
func checkToolCalls(rejections int, p Policy) verdict {
	at := p.FlagRejectionsAt
	if at <= 0 {
		at = defaultFlagRejectionsAt
	}
	v := verdict{Gate: contracts.GateToolCalls, Passed: true}
	v.Flagged = rejections >= at
	v.Detail = fmt.Sprintf("%d rejected tool calls; flagged at %d", rejections, at)
	return v
}
