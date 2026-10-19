package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// patch_parse.go reads a unified diff's headers in Go so every path and
// mode is validated before git sees the patch. It mirrors how git apply
// -p1 finds names (apply.c) closely enough that git's view of a patch the
// parser accepted is a subset of what the parser validated; the tool then
// confirms that against git's own summary, so a case the mirror gets
// wrong is a rejection, never an unvalidated write.

// Audit codes for apply_patch refusals.
const (
	codePatchArgs     = "invalid_arguments"
	codePatchInvalid  = "invalid_patch"
	codePatchRejected = "patch_rejected"
	codePatchTooLarge = "patch_too_large"
	codePatchNotFile  = "not_file"
	codePatchStaging  = "patch_staging_failed"
)

// patchModes are the only file modes a patch may name: a regular file and
// an executable one. 120000 (symlink) and 160000 (gitlink) are what make
// a patch dangerous; anything else is refused with them.
var patchModes = map[string]bool{"100644": true, "100755": true}

// patchDevNull names the missing side of a created or deleted file.
const patchDevNull = "/dev/null"

var patchHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// patchFile is one file section of a patch.
type patchFile struct {
	// oldPath and newPath are workspace-relative; empty is /dev/null or
	// a side the headers did not name.
	oldPath, newPath string
	isRename, isCopy bool
}

// parsedPatch is a patch whose every header path and mode was accepted.
type parsedPatch struct {
	files []patchFile
	// paths is every path any header named, in first-seen order.
	paths []string
	seen  map[string]bool
}

type patchParser struct {
	lines    []string
	i        int
	denylist []string
	out      parsedPatch
}

// parsePatch validates a patch's headers. Every error is a *Refusal.
func parsePatch(text string, denylist []string) (parsedPatch, error) {
	lines := strings.Split(text, "\n")
	if n := len(lines); lines[n-1] == "" {
		// The final newline terminates the last line; it is not one.
		lines = lines[:n-1]
	}
	p := &patchParser{lines: lines, denylist: denylist, out: parsedPatch{seen: map[string]bool{}}}
	for p.i < len(p.lines) {
		if err := p.next(); err != nil {
			return parsedPatch{}, err
		}
	}
	if len(p.out.files) == 0 {
		return parsedPatch{}, refuse(codePatchInvalid, "no file sections found: start each file with a diff --git line or with ---/+++ lines followed by a @@ hunk")
	}
	return p.out, nil
}

// next consumes one file section or one line of text between sections.
func (p *patchParser) next() error {
	line := p.lines[p.i]
	switch {
	case strings.HasPrefix(line, "diff --git "):
		return p.gitSection()
	case p.traditionalStart():
		return p.traditionalSection()
	case strings.HasPrefix(line, "@@ -"):
		return p.invalid("hunk without a file header")
	default:
		err := p.checkStray(line)
		p.i++
		return err
	}
}

// checkStray looks at a line git skips as commentary. Git ignores mode
// and binary lines outside a diff --git section, but a patch carrying
// them is asking for something this tool refuses, so they are refused
// wherever they appear.
func (p *patchParser) checkStray(line string) error {
	if err := p.checkBinary(line); err != nil {
		return err
	}
	for _, prefix := range []string{"old mode ", "new mode ", "new file mode ", "deleted file mode "} {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return p.checkMode(rest)
		}
	}
	return nil
}

func (p *patchParser) checkBinary(line string) error {
	if strings.HasPrefix(line, "GIT binary patch") || strings.HasPrefix(line, "Binary files ") {
		return p.reject("binary patches are not supported: write text files with write_file instead")
	}
	return nil
}

// traditionalStart reports a ---/+++ pair followed by a hunk, which is
// what git requires before it treats the pair as a file header.
func (p *patchParser) traditionalStart() bool {
	return p.i+2 < len(p.lines) &&
		strings.HasPrefix(p.lines[p.i], "--- ") &&
		strings.HasPrefix(p.lines[p.i+1], "+++ ") &&
		strings.HasPrefix(p.lines[p.i+2], "@@ -")
}

// traditionalSection reads a file section that has only ---/+++ headers.
// Git uses one of the two names for both sides; both are validated.
func (p *patchParser) traditionalSection() error {
	oldPath, oldNull, err := p.diffName(strings.TrimPrefix(p.lines[p.i], "--- "))
	if err != nil {
		return err
	}
	p.i++
	newPath, newNull, err := p.diffName(strings.TrimPrefix(p.lines[p.i], "+++ "))
	if err != nil {
		return err
	}
	if oldNull && newNull {
		return p.invalid("both sides are /dev/null")
	}
	p.i++
	p.out.files = append(p.out.files, patchFile{oldPath: oldPath, newPath: newPath})
	return p.hunks()
}

// gitSection reads a diff --git header, its extended header lines, and
// its hunks.
func (p *patchParser) gitSection() error {
	header := p.lines[p.i]
	if strings.Contains(header, `"`) {
		return p.reject("quoted (C-style) paths are not supported: use the plain path without quotes")
	}
	def := gitDefaultName(strings.TrimPrefix(header, "diff --git "))
	if def != "" {
		if err := p.validate(def); err != nil {
			return err
		}
	}
	start := p.i
	p.i++
	f := patchFile{}
	var nulls gitNulls
	for p.i < len(p.lines) && !strings.HasPrefix(p.lines[p.i], "@@ -") && !strings.HasPrefix(p.lines[p.i], "diff --git ") {
		done, err := p.gitHeaderLine(&f, &nulls, p.lines[p.i])
		if err != nil {
			return err
		}
		if done {
			break
		}
		p.i++
	}
	if p.i == start+1 {
		// Git skips a diff --git line with no header lines after it.
		return nil
	}
	if f.oldPath == "" && f.newPath == "" && !nulls.old && !nulls.new {
		if def == "" {
			return p.invalidAt(start, "diff --git header lacks a file name: use diff --git a/<path> b/<path>")
		}
		f.oldPath, f.newPath = def, def
	}
	p.out.files = append(p.out.files, f)
	return p.hunks()
}

// gitNulls records which sides a git section named as /dev/null.
type gitNulls struct{ old, new bool }

// gitHeaderLine handles one extended header line. done reports a line
// git does not recognise, which ends the header.
func (p *patchParser) gitHeaderLine(f *patchFile, nulls *gitNulls, line string) (done bool, err error) {
	if err := p.checkBinary(line); err != nil {
		return false, err
	}
	// The header lines git apply recognises (apply.c's optable).
	handlers := []struct {
		prefix string
		handle func(rest string) error
	}{
		{"--- ", func(rest string) error { return p.setDiffName(&f.oldPath, &nulls.old, rest) }},
		{"+++ ", func(rest string) error { return p.setDiffName(&f.newPath, &nulls.new, rest) }},
		{"old mode ", p.checkMode},
		{"new mode ", p.checkMode},
		{"deleted file mode ", p.checkMode},
		{"new file mode ", p.checkMode},
		{"copy from ", func(rest string) error { f.isCopy = true; return p.setRawName(&f.oldPath, rest) }},
		{"copy to ", func(rest string) error { return p.setRawName(&f.newPath, rest) }},
		{"rename old ", func(rest string) error { f.isRename = true; return p.setRawName(&f.oldPath, rest) }},
		{"rename new ", func(rest string) error { return p.setRawName(&f.newPath, rest) }},
		{"rename from ", func(rest string) error { f.isRename = true; return p.setRawName(&f.oldPath, rest) }},
		{"rename to ", func(rest string) error { return p.setRawName(&f.newPath, rest) }},
		{"similarity index ", func(string) error { return nil }},
		{"dissimilarity index ", func(string) error { return nil }},
		{"index ", p.indexLine},
	}
	for _, h := range handlers {
		if rest, ok := strings.CutPrefix(line, h.prefix); ok {
			return false, h.handle(rest)
		}
	}
	return true, nil
}

// indexLine checks the mode an "index <old>..<new> <mode>" line sets.
func (p *patchParser) indexLine(rest string) error {
	if _, mode, hasMode := strings.Cut(rest, " "); hasMode {
		return p.checkMode(mode)
	}
	return nil
}

// setDiffName applies a ---/+++ name inside a git section.
func (p *patchParser) setDiffName(dst *string, isNull *bool, rest string) error {
	name, null, err := p.diffName(rest)
	if err != nil {
		return err
	}
	if null {
		*isNull = true
		return nil
	}
	return p.assign(dst, name)
}

// setRawName applies a rename or copy name, which carries no a/ or b/
// prefix.
func (p *patchParser) setRawName(dst *string, name string) error {
	if strings.HasPrefix(name, `"`) {
		return p.reject("quoted (C-style) paths are not supported: use the plain path without quotes")
	}
	if err := p.validate(name); err != nil {
		return err
	}
	return p.assign(dst, name)
}

func (p *patchParser) assign(dst *string, name string) error {
	if *dst != "" && *dst != name {
		return p.invalid("inconsistent file names in one diff --git section")
	}
	*dst = name
	return nil
}

// diffName reads the name on a ---/+++ line: up to a tab (a timestamp
// may follow), /dev/null as itself, anything else with its a/ or b/
// prefix stripped and validated.
func (p *patchParser) diffName(rest string) (path string, null bool, err error) {
	name, _, _ := strings.Cut(rest, "\t")
	switch {
	case name == patchDevNull:
		return "", true, nil
	case strings.HasPrefix(name, `"`):
		return "", false, p.reject("quoted (C-style) paths are not supported: use the plain path without quotes")
	case strings.HasPrefix(name, "/"):
		return "", false, reject(codeInvalidPath, "patch line %d: absolute path %q: paths are relative to the repository root, written a/<path> and b/<path>", p.i+1, name)
	}
	stripped, ok := strings.CutPrefix(name, "a/")
	if !ok {
		stripped, ok = strings.CutPrefix(name, "b/")
	}
	if !ok {
		return "", false, p.invalid("path %q has no a/ or b/ prefix: write --- a/<path> and +++ b/<path>", name)
	}
	if err := p.validate(stripped); err != nil {
		return "", false, err
	}
	return stripped, false, nil
}

// validate applies the agent's path rules and records the path.
func (p *patchParser) validate(path string) error {
	if err := checkPath(path, p.denylist); err != nil {
		return reject(codeInvalidPath, "patch line %d: %v", p.i+1, err)
	}
	if !p.out.seen[path] {
		p.out.seen[path] = true
		p.out.paths = append(p.out.paths, path)
	}
	return nil
}

func (p *patchParser) checkMode(mode string) error {
	mode = strings.TrimRight(mode, " \t")
	if patchModes[mode] {
		return nil
	}
	return p.reject("file mode %q is not allowed: only 100644 and 100755; symlinks (120000) and submodules (160000) cannot be patched", mode)
}

// hunks consumes the section's hunks, counting body lines from each
// header as git does, so a body line such as "--- x" is never mistaken
// for a header.
func (p *patchParser) hunks() error {
	for p.i < len(p.lines) && strings.HasPrefix(p.lines[p.i], "@@ -") {
		if err := p.hunk(); err != nil {
			return err
		}
	}
	return nil
}

func (p *patchParser) hunk() error {
	oldN, newN, err := p.hunkCounts()
	if err != nil {
		return err
	}
	start := p.i
	p.i++
	for oldN > 0 || newN > 0 {
		if p.i >= len(p.lines) {
			return p.invalidAt(start, "hunk ends before the line counts in its @@ header are reached")
		}
		line := p.lines[p.i]
		switch {
		case line == "" || line[0] == ' ':
			oldN, newN = oldN-1, newN-1
		case line[0] == '-':
			oldN--
		case line[0] == '+':
			newN--
		case line[0] == '\\':
		default:
			return p.invalid("line in a hunk must start with ' ', '-', '+' or '\\'")
		}
		if oldN < 0 || newN < 0 {
			return p.invalidAt(start, "hunk body does not match the line counts in its @@ header")
		}
		p.i++
	}
	// "\ No newline at end of file" after the last counted line.
	for p.i < len(p.lines) && strings.HasPrefix(p.lines[p.i], `\`) {
		p.i++
	}
	return nil
}

func (p *patchParser) hunkCounts() (oldN, newN int, err error) {
	m := patchHunkHeader.FindStringSubmatch(p.lines[p.i])
	if m == nil {
		return 0, 0, p.invalid("malformed hunk header: want @@ -<start>,<count> +<start>,<count> @@")
	}
	oldN, errOld := hunkCount(m[2])
	newN, errNew := hunkCount(m[4])
	if errOld != nil || errNew != nil {
		return 0, 0, p.invalid("hunk line count out of range")
	}
	return oldN, newN, nil
}

// hunkCount reads an @@ count; an omitted count is 1.
func hunkCount(s string) (int, error) {
	if s == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > 1<<20 {
		return 0, fmt.Errorf("hunk count %q out of range", s)
	}
	return n, nil
}

func (p *patchParser) invalid(format string, args ...any) error {
	return p.invalidAt(p.i, format, args...)
}

func (p *patchParser) invalidAt(i int, format string, args ...any) error {
	return refuse(codePatchInvalid, "patch line %d: %s", i+1, fmt.Sprintf(format, args...))
}

func (p *patchParser) reject(format string, args ...any) error {
	return reject(codePatchRejected, "patch line %d: %s", p.i+1, fmt.Sprintf(format, args...))
}

// gitDefaultName mirrors git_header_name for an unquoted header: strip
// one leading component from the first name, then accept the name only
// if the rest of the line, after one separator and its own leading
// component, is the same name. A rename's header names differ, so it has
// no default; its names come from the rename lines.
func gitDefaultName(rest string) string {
	first, ok := patchStripComponent(rest)
	if !ok {
		return ""
	}
	for k := 0; k < len(first); k++ {
		if first[k] != ' ' && first[k] != '\t' {
			continue
		}
		if second, ok := patchStripComponent(first[k+1:]); ok && second == first[:k] {
			return first[:k]
		}
	}
	return ""
}

// patchStripComponent removes a non-empty leading component and its slash, as
// git's skip_tree_prefix does for -p1.
func patchStripComponent(s string) (string, bool) {
	i := strings.IndexByte(s, '/')
	if i <= 0 {
		return "", false
	}
	return s[i+1:], true
}
