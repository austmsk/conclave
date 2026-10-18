package agent

import (
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// toolEnv is what tools need from the runner's configuration.
type toolEnv struct {
	denylist          []string
	commands          Commands
	maxCommandTimeout time.Duration
}

// registry returns every tool this runner can offer. A configured command
// tool is present only when its command is configured. What a session is
// offered is the intersection of this and the role's allowlist.
func registry(env toolEnv) map[contracts.ToolName]contracts.Tool {
	tools := map[contracts.ToolName]contracts.Tool{
		contracts.ToolReadFile:   newReadFile(env),
		contracts.ToolListDir:    newListDir(env),
		contracts.ToolSearch:     newSearch(env),
		contracts.ToolWriteFile:  newWriteFile(env),
		contracts.ToolApplyPatch: newApplyPatch(env),
		contracts.ToolRunCommand: newRunCommand(env),
		contracts.ToolFinish:     newFinish(),
	}
	configured := []struct {
		name contracts.ToolName
		argv []string
	}{
		{contracts.ToolRunTests, env.commands.Test},
		{contracts.ToolRunBuild, env.commands.Build},
		{contracts.ToolRunLint, env.commands.Lint},
	}
	for _, c := range configured {
		if len(c.argv) > 0 {
			tools[c.name] = newConfiguredCommand(c.name, c.argv, env)
		}
	}
	return tools
}
