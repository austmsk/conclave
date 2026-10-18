package agent

import (
	"context"
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

var _ contracts.AgentRunner = (*Runner)(nil)

// Run executes one agent session.
func (r *Runner) Run(ctx context.Context, sb contracts.Sandbox, in contracts.SessionInput) (contracts.SessionOutput, error) {
	return contracts.SessionOutput{}, errors.New("not implemented")
}
