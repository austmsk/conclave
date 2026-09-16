package contracts

import "time"

// Role identifies an agent role. A role is data, not code: one agent loop
// executes every role, differing only by prompt, tools, limits, and schemas.
// Roles are referenced by the model router, the per-repository access matrix,
// and cost records, so the string values are stable and must not change.
type Role string

const (
	RoleImplementer      Role = "implementer"
	RoleTestWriter       Role = "test_writer"
	RoleBrief            Role = "brief"
	RolePlanner          Role = "planner"
	RoleJudge            Role = "judge"
	RolePRWriter         Role = "pr_writer"
	RoleDiscussion       Role = "discussion"
	RoleConflictResolver Role = "conflict_resolver"
)

// RoleSpec is the complete definition of a role. Adding a role means adding a
// value of this type; it does not mean writing new machinery.
type RoleSpec struct {
	Role Role

	// PromptVersion identifies the role's static instructions. Recorded on
	// every output so evaluation results remain comparable across changes.
	PromptVersion string

	// Tools is an allowlist. A tool absent from this slice is denied. This is
	// the primary containment boundary for a manipulated agent (FR-SEC3).
	Tools []ToolName

	// ReadOnly marks roles that must never mutate a workspace. Enforced in
	// addition to Tools, so a tool-list mistake cannot silently grant writes.
	ReadOnly bool

	Limits Limits
}

// Allows reports whether the role may use the named tool.
func (s RoleSpec) Allows(t ToolName) bool {
	for _, allowed := range s.Tools {
		if allowed == t {
			return true
		}
	}
	return false
}

// Limits bound a single agent session. They are enforced by the agent loop,
// never merely requested in a prompt.
type Limits struct {
	// MaxSteps caps tool calls in one session.
	MaxSteps int

	// MaxTokens caps combined input and output tokens for the session.
	MaxTokens int

	// MaxWallClock caps elapsed time for the session.
	MaxWallClock time.Duration

	// MaxRepeatErrors ends the session after this many identical command
	// failures, so a debugging spiral stops rather than burning budget.
	MaxRepeatErrors int
}
