package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/austmsk/conclave/internal/contracts"
)

// search is the search tool: it greps the workspace, hiding .git and denylisted paths.
type search struct {
	env toolEnv
}

func newSearch(env toolEnv) contracts.Tool { return &search{env: env} }

func (t *search) Name() contracts.ToolName { return contracts.ToolSearch }

func (t *search) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "description": "Search file contents in the workspace for an extended regular expression (grep -E syntax, not PCRE). Each match is printed as path:line:text, with paths relative to the workspace root; a matching line that is not valid UTF-8 (binary content) is shown without its text. Symlinks are never followed, and .git and denylisted paths are never searched. At most 200 matches are shown; the rest are counted.",
  "properties": {
    "pattern": {"type": "string", "description": "Extended regular expression to search for."},
    "path": {"type": "string", "description": "File or directory to search, relative to the workspace root. Omit or '.' for the whole workspace."},
    "glob": {"type": "string", "description": "Only report files matching this glob. Without a '/' it is matched against the file name (e.g. *.go); with one, against the whole path (e.g. internal/*/*.go). '*' does not cross '/'."}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)
}

type searchArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Glob    string `json:"glob"`
}

const (
	// maxSearchMatches is how many matches the model is shown; the rest
	// are counted.
	maxSearchMatches = 200
	// maxPatternBytes bounds the pattern, which travels in argv.
	maxPatternBytes = 4096
	// searchExitBadPattern is searchScript's exit for a pattern grep
	// cannot compile.
	searchExitBadPattern = 7
)

// searchScript walks $2 refusing symlinks, checks that grep can compile
// the pattern $1, then greps every regular file under the path. It does
// not use grep -r: BusyBox grep -r, the image's grep, opens a symlink to
// a file wherever it finds one, so it would read a link to anything in
// the container. find does not follow symlinks; -type f skips them, and
// -links 1 skips hardlinks, which the file tools refuse too. Every path
// handed to grep starts with "./" and follows "--", and the pattern
// follows -e, so neither can be read as an option. grep exits 1 when a
// batch has no match; that, and unreadable files (-s), are not errors.
const searchScript = "set -f\nrest=$2\n" + walkSnippet + `[ -e "$p" ] || exit 4
echo | grep -qE -e "$1" 2>/dev/null
[ $? -ge 2 ] && exit 7
find "$p" -name .git -prune -o -type f -links 1 -exec grep -snHE -e "$1" -- {} +
exit 0`

func (t *search) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	a, err := t.parse(args)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	argv := []string{"/bin/sh", "-c", searchScript, "sh", a.Pattern, a.Path}
	res, err := runWalkScript(ctx, sb, t.env.maxCommandTimeout, a.Path, argv, searchExitBadPattern)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	if res.ExitCode == searchExitBadPattern {
		return contracts.ToolResult{}, refuse(codeBadPattern, "pattern is not a valid extended regular expression")
	}
	return contracts.ToolResult{Output: formatMatches(res.Stdout, a.Glob, t.env.denylist)}, nil
}

// parse validates the arguments before anything reaches the sandbox. The
// returned Path is "" for the workspace root.
func (t *search) parse(args json.RawMessage) (searchArgs, error) {
	var a searchArgs
	if err := decodeArgs(args, &a); err != nil {
		return a, err
	}
	switch {
	case a.Pattern == "":
		return a, refuse(codeBadArgs, "pattern is required")
	case len(a.Pattern) > maxPatternBytes:
		return a, refuse(codeBadArgs, "pattern is longer than %d bytes", maxPatternBytes)
	case strings.ContainsRune(a.Pattern, 0):
		return a, refuse(codeBadArgs, "pattern contains a NUL byte")
	}
	if _, err := path.Match(a.Glob, ""); err != nil {
		return a, refuse(codeBadArgs, "glob %q is not a valid pattern", a.Glob)
	}
	root, err := rootPath(a.Path, t.env.denylist)
	if err != nil {
		return a, err
	}
	if root {
		a.Path = ""
	}
	return a, nil
}

// formatMatches filters grep's "./path:line:text" lines and caps them. It
// fails closed: a line it cannot parse is dropped, and so is any line
// that could be read as a hit in .git or the denylist (see matchPath).
func formatMatches(out []byte, glob string, denylist []string) string {
	var kept []string
	dropped := 0
	for _, line := range strings.Split(string(out), "\n") {
		p, ok := matchPath(line, denylist)
		if !ok || !globMatch(glob, p) {
			continue
		}
		if len(kept) == maxSearchMatches {
			dropped++
			continue
		}
		kept = append(kept, matchLine(line, p))
	}
	if len(kept) == 0 {
		return "no matches\n"
	}
	result := strings.Join(kept, "\n") + "\n"
	if dropped > 0 {
		result += fmt.Sprintf("... %d more matches truncated\n", dropped)
	}
	return result
}

// matchLine renders a kept hit as path:line:text. BusyBox grep has no
// working -I, so a match in a binary file prints raw bytes; a line that
// is not valid UTF-8 keeps its path and line number but its text is
// replaced, rather than handing the model bytes it cannot read.
func matchLine(line, p string) string {
	rest := strings.TrimPrefix(line, "./")[len(p)+1:]
	num, text, _ := strings.Cut(rest, ":")
	if !utf8.ValidString(text) {
		text = "[binary content omitted]"
	}
	return p + ":" + num + ":" + text
}

// matchPath returns the file path of one grep output line, or false when
// the line must be dropped.
//
// A line is "./" + path + ":" + digits + ":" + text, but a file name may
// itself contain ":12:", and one holding a newline splits its hits over
// two lines, so the split is ambiguous. Every reading is tried: if any
// candidate path is under .git or the denylist, the line is dropped. The
// shortest candidate is the one shown, and it must pass checkPath.
func matchPath(line string, denylist []string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "./")
	if !ok {
		return "", false
	}
	candidates := pathCandidates(rest)
	if len(candidates) == 0 {
		return "", false
	}
	for _, c := range candidates {
		if hidden(c, denylist) {
			return "", false
		}
	}
	if checkPath(candidates[0], denylist) != nil {
		return "", false
	}
	return candidates[0], true
}

// pathCandidates returns every prefix of s that ends just before a
// ":<digits>:", shortest first.
func pathCandidates(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != ':' {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j > i+1 && j < len(s) && s[j] == ':' {
			out = append(out, s[:i])
		}
	}
	return out
}

// globMatch matches a hit's path against the model's glob: the base name
// when the glob has no "/", the whole path otherwise. An empty glob
// matches everything.
func globMatch(glob, p string) bool {
	if glob == "" {
		return true
	}
	target := p
	if !strings.Contains(glob, "/") {
		target = path.Base(p)
	}
	ok, err := path.Match(glob, target)
	return err == nil && ok
}
