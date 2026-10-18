package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// maxPatchBytes caps the raw patch. contracts.ExecRequest has no stdin,
// so the patch travels base64-encoded as one argv string; Linux limits a
// single argv string to MAX_ARG_STRLEN, 128 KiB. 64 KiB encodes to
// 87,384 bytes, which fits with room to spare; 96 KiB would encode to
// exactly the limit and fail with E2BIG.
const maxPatchBytes = 64 << 10

// patchTimeout bounds each git step; applying a 64 KiB patch is quick.
const patchTimeout = 60 * time.Second

// Exit codes the patch scripts use for their own refusals, distinct from
// git's.
const (
	exitPatchSymlink = 3
	exitPatchNotFile = 4
	exitPatchStaging = 5
)

// patchPrelude is shared by every patch script. g runs git with the
// agent-writable settings that could mislead these checks pinned:
// fsmonitor and the untracked cache could make status omit a change,
// hooks could act during checkout, and core.worktree could point the
// apply elsewhere. walk refuses a symlink at any component of $1, as the
// sandbox's file scripts do, and anything but a regular file at its end.
const patchPrelude = `set -f
g() { git --git-dir=.git --work-tree=. -c core.fsmonitor=false -c core.untrackedCache=false -c core.hooksPath=/dev/null "$@"; }
walk() {
  rest=$1; p=.
  while [ -n "$rest" ]; do
    case $rest in
      */*) c=${rest%%/*}; rest=${rest#*/} ;;
      *) c=$rest; rest= ;;
    esac
    p="$p/$c"
    [ -L "$p" ] && { echo "refusing symlink: $p" >&2; exit 3; }
  done
  if [ -e "$p" ] && [ ! -f "$p" ]; then echo "not a regular file: $p" >&2; exit 4; fi
  return 0
}
`

// patchStage decodes $1 into a temporary file under the sandbox's /tmp,
// never the workspace, so the patch can never show up in the diff or in
// git status. Writing it there is safe: the bytes are the model's own
// patch, already readable to it, and the container is the boundary.
const patchStage = `f=$(mktemp) || exit 5
trap 'rm -f "$f"' EXIT
printf '%s' "$1" | base64 -d > "$f" || exit 5
`

// patchCheckScript walks every validated path, then asks git what it
// would do without doing it: the numstat (NUL-separated) and summary of
// the patch, or git's error.
const patchCheckScript = patchPrelude + `b64=$1; shift
for a; do walk "$a"; done
set -- "$b64"
` + patchStage + `g apply -p1 --check --summary --numstat -z "$f"`

// patchApplyScript applies the patch. Never --unsafe-paths, never
// --directory: git keeps every write inside the work tree.
const patchApplyScript = patchPrelude + patchStage + `g apply -p1 "$f"`

// patchStatusScript prints the work tree's status, then walks every path
// given. With no paths it is the status alone.
const patchStatusScript = patchPrelude + `g status --porcelain=v1 -z --untracked-files=all --no-renames || exit 5
for a; do walk "$a"; done`

// patchRevertScript restores tracked paths from the index.
const patchRevertScript = patchPrelude + `g checkout -- "$@"`

// applyPatch is the apply_patch tool: it applies a unified diff whose every path has been validated.
type applyPatch struct {
	env toolEnv
}

func newApplyPatch(env toolEnv) contracts.Tool { return &applyPatch{env: env} }

func (t *applyPatch) Name() contracts.ToolName { return contracts.ToolApplyPatch }

func (t *applyPatch) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"patch":{"type":"string","description":"A unified diff, as git diff writes it: paths as a/<path> and b/<path>, relative to the repository root. At most 64 KiB. Binary patches, symlinks, submodules and quoted paths are refused. The patch is checked as a whole and applied all or nothing."}},"required":["patch"],"additionalProperties":false}`)
}

// Execute validates the patch's headers, checks git's reading of the
// patch against them, applies it, and confirms afterwards that only the
// validated paths changed.
func (t *applyPatch) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	patch, err := decodePatchArgs(args)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	parsed, err := parsePatch(patch, t.env.denylist)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(patch))

	res, err := t.run(ctx, sb, patchCheckScript, append([]string{encoded}, parsed.paths...)...)
	if err != nil {
		return contracts.ToolResult{}, fmt.Errorf("checking patch: %w", err)
	}
	if err := patchScriptRefusal(res); err != nil {
		return contracts.ToolResult{}, err
	}
	if res.ExitCode != 0 {
		return patchGitFailure("git apply --check failed; nothing was applied.", res), nil
	}
	check, err := parsed.crossCheck(res.Stdout)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	return t.apply(ctx, sb, parsed, encoded, check)
}

// apply runs git apply between two status snapshots and reverts the
// change if it reached anything the headers did not name.
func (t *applyPatch) apply(ctx context.Context, sb contracts.Sandbox, parsed parsedPatch, encoded string, check gitCheck) (contracts.ToolResult, error) {
	before, err := t.status(ctx, sb, nil)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	applied, err := t.run(ctx, sb, patchApplyScript, encoded)
	if err != nil {
		return contracts.ToolResult{}, fmt.Errorf("applying patch: %w", err)
	}
	if applied.ExitCode == exitPatchStaging {
		return contracts.ToolResult{}, refuse(codePatchStaging, "could not stage the patch in the sandbox's /tmp; free space there and retry")
	}
	after, err := t.status(ctx, sb, parsed.paths)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	if err := t.verify(ctx, sb, parsed, before, after); err != nil {
		return contracts.ToolResult{}, err
	}
	if applied.ExitCode != 0 {
		return patchGitFailure("git apply failed.", applied), nil
	}
	return contracts.ToolResult{Output: check.report(applied.Stderr)}, nil
}

// patchStatusSnapshot is git status at one moment, plus whether the walk of
// the validated paths that followed it refused one.
type patchStatusSnapshot struct {
	entries []patchStatusEntry
	refused bool
}

func (t *applyPatch) status(ctx context.Context, sb contracts.Sandbox, walk []string) (patchStatusSnapshot, error) {
	res, err := t.run(ctx, sb, patchStatusScript, walk...)
	if err != nil {
		return patchStatusSnapshot{}, fmt.Errorf("reading workspace status: %w", err)
	}
	switch res.ExitCode {
	case 0, exitPatchSymlink, exitPatchNotFile:
	default:
		return patchStatusSnapshot{}, fmt.Errorf("reading workspace status: script exited %d", res.ExitCode)
	}
	entries, err := parsePatchStatus(res.Stdout)
	if err != nil {
		return patchStatusSnapshot{}, err
	}
	return patchStatusSnapshot{entries: entries, refused: res.ExitCode != 0}, nil
}

// verify confirms the apply changed only validated paths and left no
// symlink or non-file among them. Otherwise it reverts and rejects.
func (t *applyPatch) verify(ctx context.Context, sb contracts.Sandbox, parsed parsedPatch, before, after patchStatusSnapshot) error {
	unexpected := false
	for _, p := range patchChangedPaths(before.entries, after.entries) {
		if !parsed.seen[p] {
			unexpected = true
		}
	}
	if !unexpected && !after.refused {
		return nil
	}
	if err := t.revert(ctx, sb, patchNewEntries(before.entries, after.entries)); err != nil {
		return err
	}
	return reject(codePatchRejected, "after applying, the workspace held a change the patch headers did not name, or a symlink or non-file at a patched path; the change was reverted")
}

// revert undoes what git status shows as new since the apply began:
// tracked paths are restored from the index and new untracked files
// removed. Its limits: it sees status transitions, not content, so a
// file whose status was already the same before (for example one the
// agent had modified and the patch modified again) is left as it is; a
// tracked path is restored to its index version, which discards earlier
// unstaged edits to it; an untracked file the patch deleted cannot be
// restored; ignored paths are invisible to status; and empty directories
// stay, which the diff never shows. The pre-apply checks are the
// control; this is the backstop.
func (t *applyPatch) revert(ctx context.Context, sb contracts.Sandbox, entries []patchStatusEntry) error {
	var tracked, untracked []string
	for _, e := range entries {
		switch {
		case e.code == "??":
			untracked = append(untracked, e.path)
		case e.code[1] != ' ':
			tracked = append(tracked, e.path)
		}
	}
	if len(tracked) > 0 {
		res, err := t.run(ctx, sb, patchRevertScript, tracked...)
		if err := patchRevertError(res, err); err != nil {
			return fmt.Errorf("reverting patch: restoring %d tracked paths: %w", len(tracked), err)
		}
	}
	if len(untracked) > 0 {
		cmd := append([]string{"rm", "-f", "--"}, untracked...)
		res, err := sb.Exec(ctx, contracts.ExecRequest{Command: cmd, Timeout: t.timeout()})
		if err := patchRevertError(res, err); err != nil {
			return fmt.Errorf("reverting patch: removing %d new files: %w", len(untracked), err)
		}
	}
	return nil
}

// patchRevertError treats a failed revert step as a sandbox failure: the
// workspace is in a state the tool cannot vouch for.
func patchRevertError(res contracts.ExecResult, err error) error {
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d", res.ExitCode)
	}
	return nil
}

func (t *applyPatch) run(ctx context.Context, sb contracts.Sandbox, script string, args ...string) (contracts.ExecResult, error) {
	cmd := append([]string{"/bin/sh", "-c", script, "sh"}, args...)
	return sb.Exec(ctx, contracts.ExecRequest{Command: cmd, Timeout: t.timeout()})
}

func (t *applyPatch) timeout() time.Duration {
	if t.env.maxCommandTimeout > 0 && t.env.maxCommandTimeout < patchTimeout {
		return t.env.maxCommandTimeout
	}
	return patchTimeout
}

// patchScriptRefusal turns the check script's own exit codes into
// refusals.
func patchScriptRefusal(res contracts.ExecResult) error {
	switch res.ExitCode {
	case exitPatchSymlink:
		return reject(codeInvalidPath, "a path in the patch is or goes through a symlink; patches may only touch regular files reached without symlinks")
	case exitPatchNotFile:
		return refuse(codePatchNotFile, "a path in the patch exists but is not a regular file (a directory or a submodule)")
	case exitPatchStaging:
		return refuse(codePatchStaging, "could not stage the patch in the sandbox's /tmp; free space there and retry")
	}
	return nil
}

// patchGitFailure reports git's own refusal as a command result: the
// patch did not apply, which the model fixes by rewriting it.
func patchGitFailure(note string, res contracts.ExecResult) contracts.ToolResult {
	return contracts.ToolResult{Output: note + "\n" + string(res.Stderr) + string(res.Stdout), ExitCode: res.ExitCode}
}

// decodePatchArgs reads {"patch": string} strictly.
func decodePatchArgs(raw json.RawMessage) (string, error) {
	var args struct {
		Patch *string `json:"patch"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return "", refuse(codePatchArgs, `arguments must be {"patch": "<unified diff>"}: %v`, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", refuse(codePatchArgs, `arguments must be one JSON object, {"patch": "<unified diff>"}`)
	}
	if args.Patch == nil || *args.Patch == "" {
		return "", refuse(codePatchArgs, `"patch" is required`)
	}
	patch := *args.Patch
	if !strings.HasSuffix(patch, "\n") {
		// git needs every patch line terminated; a missing final newline
		// is the commonest slip and changes nothing else.
		patch += "\n"
	}
	if len(patch) > maxPatchBytes {
		return "", refuse(codePatchTooLarge, "patch is %d bytes, over the %d-byte limit: split it into several apply_patch calls, or use write_file for whole files", len(patch), maxPatchBytes)
	}
	return patch, nil
}

// gitCheck is git's reading of the patch from apply --numstat.
type gitCheck struct {
	files []gitNumstat
}

type gitNumstat struct {
	added, deleted, name string
}

func (c gitCheck) report(warnings []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Applied patch to %d file(s):\n", len(c.files))
	for _, f := range c.files {
		fmt.Fprintf(&b, "  %s (+%s -%s)\n", f.name, f.added, f.deleted)
	}
	if len(warnings) > 0 {
		b.WriteString("git apply warnings:\n")
		b.Write(warnings)
	}
	return b.String()
}

var (
	summaryFileMode   = regexp.MustCompile(`^ (?:create|delete) mode ([0-7]{6}) (.+)$`)
	summaryModeChange = regexp.MustCompile(`^ mode change ([0-7]{6}) => ([0-7]{6})(?: (.+))?$`)
	summaryRewrite    = regexp.MustCompile(`^ rewrite (.+) \(\d+%\)$`)
	summaryRenameCopy = regexp.MustCompile(`^ (rename|copy) (.+) \(\d+%\)$`)
)

// crossCheck compares git's --numstat -z --summary output with what the
// parser validated. Any file git sees that the parser did not, any path
// or mode it names that the parser did not accept, and any binary file
// is a rejection: nothing has been applied yet. The messages are fixed
// text, never git's output.
func (pp parsedPatch) crossCheck(out []byte) (gitCheck, error) {
	mismatch := func(what string) error {
		return reject(codePatchRejected, "git read this patch differently from the header check (%s), so nothing was applied; write plain diff --git a/<path> b/<path> headers with ---/+++ lines", what)
	}
	fields := strings.Split(string(out), "\x00")
	summary := fields[len(fields)-1]
	var check gitCheck
	for _, rec := range fields[:len(fields)-1] {
		parts := strings.SplitN(rec, "\t", 3)
		switch {
		case len(parts) != 3:
			return gitCheck{}, mismatch("unreadable numstat")
		case parts[0] == "-" || parts[1] == "-":
			return gitCheck{}, reject(codePatchRejected, "git reads part of this patch as binary; binary patches are not supported")
		case !pp.seen[parts[2]]:
			return gitCheck{}, mismatch("a path git would write was not in the headers")
		}
		check.files = append(check.files, gitNumstat{added: parts[0], deleted: parts[1], name: parts[2]})
	}
	if len(check.files) != len(pp.files) {
		return gitCheck{}, mismatch(fmt.Sprintf("git sees %d files, the headers %d", len(check.files), len(pp.files)))
	}
	for _, line := range strings.Split(summary, "\n") {
		if line == "" {
			continue
		}
		if !pp.summaryLineOK(line) {
			return gitCheck{}, mismatch("a path or file mode in git's summary was not in the headers")
		}
	}
	return check, nil
}

// summaryLineOK accepts a git apply --summary line only if every path in
// it was validated and every mode is allowed.
func (pp parsedPatch) summaryLineOK(line string) bool {
	if m := summaryFileMode.FindStringSubmatch(line); m != nil {
		return patchModes[m[1]] && pp.seen[m[2]]
	}
	if m := summaryModeChange.FindStringSubmatch(line); m != nil {
		// Without a name it follows the rename or rewrite line it
		// belongs to, which is checked on its own.
		return patchModes[m[1]] && patchModes[m[2]] && (m[3] == "" || pp.seen[m[3]])
	}
	if m := summaryRenameCopy.FindStringSubmatch(line); m != nil {
		for _, f := range pp.files {
			if (f.isRename && m[1] == "rename" || f.isCopy && m[1] == "copy") && renameSummary(f.oldPath, f.newPath) == m[2] {
				return true
			}
		}
		return false
	}
	if m := summaryRewrite.FindStringSubmatch(line); m != nil {
		return pp.seen[m[1]]
	}
	return false
}

// renameSummary formats a rename as git apply --summary does, folding a
// shared leading directory into "dir/{old => new}".
func renameSummary(oldPath, newPath string) string {
	o, n := oldPath, newPath
	for {
		so, sn := strings.IndexByte(o, '/'), strings.IndexByte(n, '/')
		if so < 0 || sn < 0 || so != sn || o[:so] != n[:sn] {
			break
		}
		o, n = o[so+1:], n[sn+1:]
	}
	if prefix := oldPath[:len(oldPath)-len(o)]; prefix != "" {
		return prefix + "{" + o + " => " + n + "}"
	}
	return o + " => " + n
}

// patchStatusEntry is one git status --porcelain -z entry.
type patchStatusEntry struct {
	code, path string
}

// parsePatchStatus reads git status --porcelain=v1 -z. A rename or copy entry
// carries its source in the next field; that is recorded as an entry of
// its own so it is checked too.
func parsePatchStatus(out []byte) ([]patchStatusEntry, error) {
	fields := strings.Split(string(out), "\x00")
	var entries []patchStatusEntry
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		if len(f) < 4 || f[2] != ' ' {
			return nil, errors.New("reading workspace status: unexpected git status output")
		}
		entries = append(entries, patchStatusEntry{code: f[:2], path: f[3:]})
		if (f[0] == 'R' || f[0] == 'C') && i+1 < len(fields) {
			i++
			entries = append(entries, patchStatusEntry{code: f[:2] + "<", path: fields[i]})
		}
	}
	return entries, nil
}

// patchChangedPaths returns the path of every entry present in only one of
// the two snapshots.
func patchChangedPaths(before, after []patchStatusEntry) []string {
	var paths []string
	for _, e := range patchNewEntries(before, after) {
		paths = append(paths, e.path)
	}
	for _, e := range patchNewEntries(after, before) {
		paths = append(paths, e.path)
	}
	return paths
}

// patchNewEntries returns the entries of b that a does not hold.
func patchNewEntries(a, b []patchStatusEntry) []patchStatusEntry {
	had := make(map[patchStatusEntry]bool, len(a))
	for _, e := range a {
		had[e] = true
	}
	var out []patchStatusEntry
	for _, e := range b {
		if !had[e] {
			out = append(out, e)
		}
	}
	return out
}
