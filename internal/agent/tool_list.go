package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

// listDir is the list_dir tool: it lists a workspace directory, hiding .git and denylisted entries.
type listDir struct {
	env toolEnv
}

func newListDir(env toolEnv) contracts.Tool { return &listDir{env: env} }

func (t *listDir) Name() contracts.ToolName { return contracts.ToolListDir }

func (t *listDir) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *listDir) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	return contracts.ToolResult{}, errors.New("not implemented")
}
