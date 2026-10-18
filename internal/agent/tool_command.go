package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

// runCommand is the run_command tool: it runs a model-supplied command, the audited escape hatch.
type runCommand struct {
	env toolEnv
}

func newRunCommand(env toolEnv) contracts.Tool { return &runCommand{env: env} }

func (t *runCommand) Name() contracts.ToolName { return contracts.ToolRunCommand }

func (t *runCommand) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *runCommand) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	return contracts.ToolResult{}, errors.New("not implemented")
}

// configuredCommand runs a repository's configured command (FR-I4). It
// takes no arguments from the model.
type configuredCommand struct {
	name contracts.ToolName
	argv []string
}

func newConfiguredCommand(name contracts.ToolName, argv []string, env toolEnv) contracts.Tool {
	return &configuredCommand{name: name, argv: argv}
}

func (t *configuredCommand) Name() contracts.ToolName { return t.name }

func (t *configuredCommand) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *configuredCommand) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	return contracts.ToolResult{}, errors.New("not implemented")
}
