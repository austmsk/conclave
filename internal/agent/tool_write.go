package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/austmsk/conclave/internal/contracts"
)

// writeFile is the write_file tool: it replaces a workspace file.
type writeFile struct {
	env toolEnv
}

func newWriteFile(env toolEnv) contracts.Tool { return &writeFile{env: env} }

func (t *writeFile) Name() contracts.ToolName { return contracts.ToolWriteFile }

func (t *writeFile) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "description": "Create or replace a file in the workspace with the given content, creating missing parent directories. Paths are relative to the workspace root; absolute paths, '..', anything under .git, and denylisted paths are refused, as is any path through a symlink.",
  "properties": {
    "path": {"type": "string", "description": "File path relative to the workspace root, e.g. src/main.go."},
    "content": {"type": "string", "description": "The file's complete new content."}
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`)
}

type writeFileArgs struct {
	Path    string  `json:"path"`
	Content *string `json:"content"`
}

func (t *writeFile) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	var a writeFileArgs
	if err := decodeArgs(args, &a); err != nil {
		return contracts.ToolResult{}, err
	}
	switch {
	case a.Path == "":
		return contracts.ToolResult{}, refuse(codeBadArgs, "path is required")
	case a.Content == nil:
		return contracts.ToolResult{}, refuse(codeBadArgs, "content is required")
	}
	if err := checkPath(a.Path, t.env.denylist); err != nil {
		return contracts.ToolResult{}, err
	}
	if err := sb.WriteFile(ctx, a.Path, []byte(*a.Content)); err != nil {
		return contracts.ToolResult{}, fmt.Errorf("write_file %q: %w", a.Path, err)
	}
	return contracts.ToolResult{Output: fmt.Sprintf("wrote %d bytes to %s", len(*a.Content), a.Path)}, nil
}
