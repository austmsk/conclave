package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

// Audit codes for the file tools' own refusals.
const (
	codeBadArgs      = "invalid_arguments"
	codeNotFound     = "not_found"
	codeNotDirectory = "not_directory"
	codeBadRange     = "invalid_line_range"
	codeBadPattern   = "invalid_pattern"
	codeTimedOut     = "timed_out"
)

// checkPath applies sandbox.CheckPath and then the denylist. Every refusal
// wraps sandbox.ErrInvalidPath, so the runner counts it as a rejection.
func checkPath(p string, denylist []string) error {
	if err := sandbox.CheckPath(p); err != nil {
		return err
	}
	if entry, ok := denied(p, denylist); ok {
		return fmt.Errorf("%w: %q is under the denylisted path %q", sandbox.ErrInvalidPath, p, entry)
	}
	return nil
}

// denied reports the first denylist entry whose components appear as a
// contiguous run of p's components, at any depth: "secrets" denies
// "secrets/key.pem" and "app/secrets/key.pem" but not "secretsfoo";
// "config/prod.yaml" denies "deploy/config/prod.yaml" but not
// "config/prod.yaml.example". Matching anywhere rather than anchored at
// the root fails closed: an entry denies too much rather than too little.
//
// Components are compared by their sandbox.FoldName, so case and the
// spellings HFS+ and NTFS treat as equal all match. The sandbox's own
// filesystem is case-sensitive, but the branch is checked out by the
// owner, maybe on macOS or Windows, where "Secrets/key.pem" written in
// the sandbox lands in secrets/. It is the same reason CheckPath folds
// .git. Entries are literal; glob characters in them match themselves.
func denied(p string, denylist []string) (string, bool) {
	comps := foldComponents(p)
	for _, entry := range denylist {
		run := foldComponents(entry)
		if len(run) > 0 && containsRun(comps, run) {
			return entry, true
		}
	}
	return "", false
}

// hidden reports whether p names something under .git (in any spelling
// CheckPath refuses) or the denylist. It is the filter for paths the
// sandbox reports back, such as search hits, which have not been through
// CheckPath.
func hidden(p string, denylist []string) bool {
	for _, c := range strings.Split(p, "/") {
		if sandbox.IsGitName(c) {
			return true
		}
	}
	_, ok := denied(p, denylist)
	return ok
}

// foldComponents splits p on "/" and folds each component, dropping
// empty ones so "secrets/" and "/secrets" mean "secrets".
func foldComponents(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			out = append(out, sandbox.FoldName(c))
		}
	}
	return out
}

func containsRun(comps, run []string) bool {
	for i := 0; i+len(run) <= len(comps); i++ {
		if equalRun(comps[i:i+len(run)], run) {
			return true
		}
	}
	return false
}

func equalRun(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// decodeArgs decodes a tool's JSON arguments strictly. Anything but a
// single object of the declared fields is a refusal the model can correct.
func decodeArgs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return refuse(codeBadArgs, "arguments are not valid: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return refuse(codeBadArgs, "arguments have trailing data after the JSON object")
	}
	return nil
}

// walkSnippet sets p to "." followed by each component of $rest in turn,
// exiting walkExitSymlink at the first that is a symlink, the same walk
// the Docker backend's file scripts do. Commands run in the workspace, so
// "." is its root; the path is only ever expanded quoted, under set -f.
// It refuses a symlink anywhere on the path, including the last
// component, so neither a listing nor a search starts outside the
// workspace.
const walkSnippet = `p=.
while [ -n "$rest" ]; do
  case $rest in
    */*) c=${rest%%/*}; rest=${rest#*/} ;;
    *) c=$rest; rest= ;;
  esac
  p="$p/$c"
  [ -L "$p" ] && exit 3
done
`

// Exit codes the walking scripts use for their own refusals.
const (
	walkExitSymlink = 3
	walkExitMissing = 4
)

// runWalkScript runs a script built on walkSnippet with the given
// arguments and maps the walk's refusals. It returns the result for the
// caller to read when the script exited 0, or with any exit code the
// caller lists in own.
func runWalkScript(ctx context.Context, sb contracts.Sandbox, timeout time.Duration, rel string, argv []string, own ...int) (contracts.ExecResult, error) {
	res, err := sb.Exec(ctx, contracts.ExecRequest{Command: argv, Timeout: timeout})
	if err != nil {
		return res, fmt.Errorf("walking %q: %w", rel, err)
	}
	if res.TimedOut {
		return res, refuse(codeTimedOut, "timed out after %v", timeout)
	}
	for _, code := range own {
		if res.ExitCode == code {
			return res, nil
		}
	}
	switch res.ExitCode {
	case 0:
		return res, nil
	case walkExitSymlink:
		return res, fmt.Errorf("%w: %q passes through a symlink", sandbox.ErrInvalidPath, displayPath(rel))
	case walkExitMissing:
		return res, refuse(codeNotFound, "%q does not exist", displayPath(rel))
	}
	// The script's stderr is not vetted, so it is not passed on.
	return res, fmt.Errorf("walking %q: script exited %d", rel, res.ExitCode)
}

// rootPath reports whether a directory argument means the workspace root,
// and otherwise validates it.
func rootPath(p string, denylist []string) (bool, error) {
	if p == "" || p == "." {
		return true, nil
	}
	return false, checkPath(p, denylist)
}

func displayPath(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// joinPath joins a workspace-relative directory and a name; dir "" is the
// root.
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
