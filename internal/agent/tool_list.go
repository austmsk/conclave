package agent

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
)

// listDir is the list_dir tool: it lists a workspace directory, hiding .git and denylisted entries.
type listDir struct {
	env toolEnv
}

func newListDir(env toolEnv) contracts.Tool { return &listDir{env: env} }

func (t *listDir) Name() contracts.ToolName { return contracts.ToolListDir }

func (t *listDir) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "description": "List the entries of a workspace directory, one per line, sorted by name. Directories end in '/', symlinks in '@'; symlinks are listed but never followed, and a path through one is refused. The path is relative to the workspace root; omit it, or pass '.', for the root itself. .git, denylisted entries, and names the other file tools would refuse are not shown. An empty directory lists nothing.",
  "properties": {
    "path": {"type": "string", "description": "Directory relative to the workspace root, e.g. src. Omit or '.' for the root."}
  },
  "additionalProperties": false
}`)
}

type listDirArgs struct {
	Path string `json:"path"`
}

// listExitNotDir is listScript's exit for a path that is not a directory.
const listExitNotDir = 5

// listScript walks $1 refusing symlinks, then prints one record per entry
// of the directory it names: a type letter (d directory, l symlink, f
// anything else) and the name, NUL-terminated, since a name may hold any
// byte but NUL and "/". Globbing is switched back on only for the listing
// itself, inside the directory, where it cannot match anything outside.
const listScript = "set -f\nrest=$1\n" + walkSnippet + `[ -e "$p" ] || exit 4
[ -d "$p" ] || exit 5
cd -- "$p" || exit 5
set +f
for e in * .*; do
  case $e in .|..) continue ;; esac
  if [ -L "$e" ]; then t=l
  elif [ -d "$e" ]; then t=d
  elif [ -e "$e" ]; then t=f
  else continue
  fi
  printf '%s%s\0' "$t" "$e"
done
exit 0`

func (t *listDir) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	var a listDirArgs
	if err := decodeArgs(args, &a); err != nil {
		return contracts.ToolResult{}, err
	}
	root, err := rootPath(a.Path, t.env.denylist)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	rel := a.Path
	if root {
		rel = ""
	}
	argv := []string{"/bin/sh", "-c", listScript, "sh", rel}
	res, err := runWalkScript(ctx, sb, t.env.maxCommandTimeout, rel, argv, listExitNotDir)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	if res.ExitCode == listExitNotDir {
		return contracts.ToolResult{}, refuse(codeNotDirectory, "%q is not a directory", rel)
	}
	return contracts.ToolResult{Output: formatListing(rel, res.Stdout, t.env.denylist)}, nil
}

// formatListing turns the script's records into the model's listing. It
// fails closed: a record it cannot read, and any entry whose path the
// other tools would refuse, is left out.
func formatListing(dir string, out []byte, denylist []string) string {
	var lines []string
	records := strings.Split(string(out), "\x00")
	// Every record ends in NUL, so the last piece is empty or cut short.
	for _, rec := range records[:len(records)-1] {
		if len(rec) < 2 {
			continue
		}
		kind, name := rec[0], rec[1:]
		if strings.Contains(name, "/") || checkPath(joinPath(dir, name), denylist) != nil {
			continue
		}
		switch kind {
		case 'd':
			lines = append(lines, name+"/")
		case 'l':
			lines = append(lines, name+"@")
		case 'f':
			lines = append(lines, name)
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
