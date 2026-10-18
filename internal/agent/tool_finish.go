package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

// finish is the finish tool: it ends the session with a completion report.
type finish struct {
}

func newFinish() contracts.Tool { return &finish{} }

func (t *finish) Name() contracts.ToolName { return contracts.ToolFinish }

func (t *finish) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *finish) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	return contracts.ToolResult{}, errors.New("not implemented")
}
