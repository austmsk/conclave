package contracts

import (
	"context"
	"encoding/json"
)

// ToolName identifies a tool available to an agent. Values are stable: they
// appear in role allowlists, tool-call audit rows, and evaluation data.
type ToolName string

const (
	ToolReadFile ToolName = "read_file"
	ToolListDir  ToolName = "list_dir"
	ToolSearch   ToolName = "search"

	ToolWriteFile  ToolName = "write_file"
	ToolApplyPatch ToolName = "apply_patch"

	// Configured commands take no arguments from the model; the command
	// string comes from repository configuration. Normal work uses these.
	ToolRunTests ToolName = "run_tests"
	ToolRunBuild ToolName = "run_build"
	ToolRunLint  ToolName = "run_lint"

	// ToolRunCommand is the general escape hatch. Because ordinary work uses
	// the configured commands above, every use of this tool is an audit
	// outlier by construction (FR-SEC24).
	ToolRunCommand ToolName = "run_command"

	// ToolRequestDependency proposes a dependency. Platform code performs the
	// checks and the install; the agent never runs a package manager during
	// the agent phase (FR-SEC14).
	ToolRequestDependency ToolName = "request_dependency"

	// ToolFinish ends the session with a structured report. Completion is
	// signalled explicitly rather than inferred from an absent tool call.
	ToolFinish ToolName = "finish"
)

// ToolCall is a model's request to invoke a tool.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      ToolName        `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult is the outcome of executing a ToolCall. It is recorded in full by
// the worker (never from inside the sandbox) before being returned to the
// model, so an agent cannot alter its own audit trail.
type ToolResult struct {
	CallID string `json:"call_id"`

	// Output is what the model sees. It may be truncated; the untruncated
	// output is stored in the blob store at ArtifactKey.
	Output      string `json:"output"`
	Truncated   bool   `json:"truncated"`
	ArtifactKey string `json:"artifact_key,omitempty"`

	// ExitCode applies to command tools; zero otherwise.
	ExitCode int `json:"exit_code"`

	// Error is set when the tool refused or failed. A refusal (for example a
	// rejected path) is a normal result, not a transport error: the model
	// sees it and can correct course.
	Error string `json:"error,omitempty"`
}

// Tool is one capability an agent can invoke. Implementations are pure with
// respect to policy: they assume the caller has already checked the role's
// allowlist, and they enforce their own argument validation.
type Tool interface {
	Name() ToolName

	// Schema returns the JSON Schema for Arguments, sent to the model.
	Schema() json.RawMessage

	// Execute runs the tool against a sandbox. Implementations must validate
	// paths through the workspace root handle (FR-SEC29) and must never
	// receive or emit credentials.
	Execute(ctx context.Context, sb Sandbox, args json.RawMessage) (ToolResult, error)
}
