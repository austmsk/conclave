package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
)

// readFile is the read_file tool: it returns a workspace file, optionally a line range.
type readFile struct {
	env toolEnv
}

func newReadFile(env toolEnv) contracts.Tool { return &readFile{env: env} }

func (t *readFile) Name() contracts.ToolName { return contracts.ToolReadFile }

func (t *readFile) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "description": "Read a file in the workspace. Paths are relative to the workspace root; absolute paths, '..', anything under .git, and denylisted paths are refused, as is any path through a symlink. Without a range the whole file is returned. start_line and end_line select an inclusive, 1-based line range; either may be omitted, and an end_line past the end of the file reads to the end.",
  "properties": {
    "path": {"type": "string", "description": "File path relative to the workspace root, e.g. src/main.go."},
    "start_line": {"type": "integer", "minimum": 1, "description": "First line to return, counting from 1."},
    "end_line": {"type": "integer", "minimum": 1, "description": "Last line to return, inclusive."}
  },
  "required": ["path"],
  "additionalProperties": false
}`)
}

type readFileArgs struct {
	Path      string `json:"path"`
	StartLine *int   `json:"start_line"`
	EndLine   *int   `json:"end_line"`
}

func (t *readFile) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	var a readFileArgs
	if err := decodeArgs(args, &a); err != nil {
		return contracts.ToolResult{}, err
	}
	if a.Path == "" {
		return contracts.ToolResult{}, refuse(codeBadArgs, "path is required")
	}
	if err := checkPath(a.Path, t.env.denylist); err != nil {
		return contracts.ToolResult{}, err
	}
	if err := checkRange(a.StartLine, a.EndLine); err != nil {
		return contracts.ToolResult{}, err
	}
	data, err := sb.ReadFile(ctx, a.Path)
	if err != nil {
		return contracts.ToolResult{}, fmt.Errorf("read_file %q: %w", a.Path, err)
	}
	if a.StartLine == nil && a.EndLine == nil {
		return contracts.ToolResult{Output: string(data)}, nil
	}
	out, err := lineRange(string(data), a.StartLine, a.EndLine)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	return contracts.ToolResult{Output: out}, nil
}

// checkRange validates a line range before anything is read.
func checkRange(start, end *int) error {
	switch {
	case start != nil && *start < 1:
		return refuse(codeBadRange, "start_line %d must be at least 1", *start)
	case end != nil && *end < 1:
		return refuse(codeBadRange, "end_line %d must be at least 1", *end)
	case start != nil && end != nil && *end < *start:
		return refuse(codeBadRange, "end_line %d is before start_line %d", *end, *start)
	}
	return nil
}

// lineRange returns lines start..end of s, 1-based and inclusive, with
// their line endings as they are in the file. A missing start is 1; a
// missing end, or one past the last line, is the last line. A final line
// without a newline counts; the empty string after a final newline does
// not.
func lineRange(s string, start, end *int) (string, error) {
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	first, last := 1, len(lines)
	if start != nil {
		first = *start
	}
	if end != nil && *end < last {
		last = *end
	}
	if first > len(lines) {
		return "", refuse(codeBadRange, "start_line %d is past the end of the file, which has %d lines", first, len(lines))
	}
	return strings.Join(lines[first-1:last], ""), nil
}
