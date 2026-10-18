package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

// writeFile is the write_file tool: it replaces a workspace file.
type writeFile struct {
	env toolEnv
}

func newWriteFile(env toolEnv) contracts.Tool { return &writeFile{env: env} }

func (t *writeFile) Name() contracts.ToolName { return contracts.ToolWriteFile }

func (t *writeFile) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *writeFile) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	return contracts.ToolResult{}, errors.New("not implemented")
}
