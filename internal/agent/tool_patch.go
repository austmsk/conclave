package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

// applyPatch is the apply_patch tool: it applies a unified diff whose every path has been validated.
type applyPatch struct {
	env toolEnv
}

func newApplyPatch(env toolEnv) contracts.Tool { return &applyPatch{env: env} }

func (t *applyPatch) Name() contracts.ToolName { return contracts.ToolApplyPatch }

func (t *applyPatch) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *applyPatch) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	return contracts.ToolResult{}, errors.New("not implemented")
}
