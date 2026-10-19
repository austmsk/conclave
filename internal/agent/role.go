package agent

import (
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// Implementer is the Milestone 1 implementer role (docs/agent-roles.md).
// Its PromptVersion names systemPrompt; change one and change the other.
//
// It has every Milestone 1 tool. request_dependency waits for Milestone
// 3; until then a dependency the change needs goes in the finish report.
// The configured command tools are listed, so a repository must configure
// test, build and lint commands, or the runner refuses the role.
//
// The limits are docs/agent-roles.md's starting values, to be revised with
// Milestone 2 data:
//   - MaxSteps 60 tool calls.
//   - MaxTokens 2,000,000 input and output tokens. A session resends its
//     history with each request, so 60 steps at an average of about 30k
//     tokens a request is about 1.8M. The binding cap is usually the
//     attempt's token budget in the router scope, which comes from
//     repository configuration; this is the role's ceiling under it.
//   - MaxWallClock 20 minutes.
//   - MaxRepeatErrors 3 identical failures.
var Implementer = contracts.RoleSpec{
	Role:          contracts.RoleImplementer,
	PromptVersion: "implementer-v1",
	Tools: []contracts.ToolName{
		contracts.ToolReadFile,
		contracts.ToolListDir,
		contracts.ToolSearch,
		contracts.ToolWriteFile,
		contracts.ToolApplyPatch,
		contracts.ToolRunTests,
		contracts.ToolRunBuild,
		contracts.ToolRunLint,
		contracts.ToolRunCommand,
		contracts.ToolFinish,
	},
	ReadOnly: false,
	Limits: contracts.Limits{
		MaxSteps:        60,
		MaxTokens:       2_000_000,
		MaxWallClock:    20 * time.Minute,
		MaxRepeatErrors: 3,
	},
}
