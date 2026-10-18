package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

// readFile is the read_file tool: it returns a workspace file, optionally a line range.
type readFile struct {
	env toolEnv
}

func newReadFile(env toolEnv) contracts.Tool { return &readFile{env: env} }

func (t *readFile) Name() contracts.ToolName { return contracts.ToolReadFile }

func (t *readFile) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *readFile) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	return contracts.ToolResult{}, errors.New("not implemented")
}
