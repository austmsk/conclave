package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

// search is the search tool: it greps the workspace, hiding .git and denylisted paths.
type search struct {
	env toolEnv
}

func newSearch(env toolEnv) contracts.Tool { return &search{env: env} }

func (t *search) Name() contracts.ToolName { return contracts.ToolSearch }

func (t *search) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *search) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	return contracts.ToolResult{}, errors.New("not implemented")
}
