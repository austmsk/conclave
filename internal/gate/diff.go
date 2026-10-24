package gate

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// diff.go parses the recorded diff: the output of git add -A -N . && git
// diff --binary --no-color --no-ext-diff <base>, produced inside the
// agent's sandbox under a .git the agent could edit. It is
// attacker-controlled input, so the parser accepts only the shapes git
// itself emits with default configuration and refuses everything else:
// a diff it misreads could hide a path from the static gates. Staging
// separately requires the same bytes to round-trip through an isolated
// git, so a diff git would not produce never reaches a later step.

// ErrMalformedDiff reports diff bytes the parser refuses. The diff comes
// from the agent's sandbox, so anything unexpected fails closed.
var ErrMalformedDiff = errors.New("malformed diff")

// ChangeKind is what a diff did to one file.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeDeleted  ChangeKind = "deleted"
	ChangeModified ChangeKind = "modified"
	ChangeRenamed  ChangeKind = "renamed"
	ChangeCopied   ChangeKind = "copied"
)

// FileChange is one file's entry in a git diff --binary.
type FileChange struct {
	Kind ChangeKind

	// OldPath and NewPath are unquoted, slash-separated, without the a/
	// and b/ prefixes. OldPath is empty for an added file and NewPath for
	// a deleted one.
	OldPath string
	NewPath string

	// OldMode and NewMode are git modes such as "100644", "100755",
	// "120000" (symlink) or "160000" (gitlink); empty when absent.
	OldMode string
	NewMode string

	// Binary is set for a GIT binary patch or "Binary files differ".
	Binary bool

	// Added and Removed are the text lines, without their +/- prefix.
	Added   []string
	Removed []string

	// AddedAt[i] is the line number of Added[i] in the new file, and
	// RemovedAt[i] that of Removed[i] in the old file, from 1 and
	// strictly increasing, read from the hunk headers. They locate
	// evidence and tell which changed lines are adjacent.
	AddedAt   []int
	RemovedAt []int
}

// Parser bounds. MaxArtifactBytes already caps what Freeze accepts; these
// keep a hostile diff within it from costing more than linear work.
const (
	maxDiffFiles     = 10000
	maxDiffLineBytes = 1 << 20
	maxDiffLines     = 1 << 21
)

const devNull = "/dev/null"

var (
	hunkHeader  = regexp.MustCompile(`^@@ -(\d{1,9})(?:,(\d{1,9}))? \+(\d{1,9})(?:,(\d{1,9}))? @@(?: .*)?$`)
	indexLine   = regexp.MustCompile(`^index ([0-9a-f]{7,64})\.\.([0-9a-f]{7,64})(?: ([0-9]{6}))?$`)
	binarySize  = regexp.MustCompile(`^(?:literal|delta) (0|[1-9][0-9]{0,11})$`)
	percentLine = regexp.MustCompile(`^(?:0|[1-9][0-9]?|100)%$`)
)

// gitModes are the only modes git records for a file.
var gitModes = map[string]bool{"100644": true, "100755": true, "120000": true, "160000": true}

const noNewline = `\ No newline at end of file`

// ParseDiff parses git diff --binary output strictly and within bounds.
// An empty diff is no changes. Every refusal wraps ErrMalformedDiff and
// names a line number, never the line's content.
func ParseDiff(diff []byte) ([]FileChange, error) {
	if len(diff) == 0 {
		return nil, nil
	}
	if len(diff) > MaxArtifactBytes {
		return nil, fmt.Errorf("%w: %d bytes, cap %d", ErrMalformedDiff, len(diff), MaxArtifactBytes)
	}
	if diff[len(diff)-1] != '\n' {
		return nil, fmt.Errorf("%w: does not end with a newline", ErrMalformedDiff)
	}
	lines := strings.Split(string(diff[:len(diff)-1]), "\n")
	if len(lines) > maxDiffLines {
		return nil, fmt.Errorf("%w: more than %d lines", ErrMalformedDiff, maxDiffLines)
	}
	for i, l := range lines {
		if len(l) > maxDiffLineBytes {
			return nil, fmt.Errorf("%w: line %d: longer than %d bytes", ErrMalformedDiff, i+1, maxDiffLineBytes)
		}
	}
	p := diffParser{lines: lines}
	var out []FileChange
	for !p.done() {
		if len(out) == maxDiffFiles {
			return nil, fmt.Errorf("%w: more than %d files", ErrMalformedDiff, maxDiffFiles)
		}
		fc, err := p.file()
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %w", ErrMalformedDiff, p.i, err)
		}
		out = append(out, fc)
	}
	return out, nil
}

// diffParser walks the diff one line at a time. i is the index of the
// next unread line, so after next() it is the 1-based number of the line
// just read.
type diffParser struct {
	lines []string
	i     int
}

func (p *diffParser) done() bool { return p.i >= len(p.lines) }

func (p *diffParser) peek() (string, bool) {
	if p.done() {
		return "", false
	}
	return p.lines[p.i], true
}

func (p *diffParser) next() (string, bool) {
	l, ok := p.peek()
	if ok {
		p.i++
	}
	return l, ok
}

// atFileEnd reports whether the current file's entry has ended.
func (p *diffParser) atFileEnd() bool {
	l, ok := p.peek()
	return !ok || strings.HasPrefix(l, "diff --git ")
}

// fileEntry collects one file's headers before they are reconciled.
type fileEntry struct {
	gitLine string

	oldMode, newMode     string
	newFileMode, delMode string
	indexMode            string
	renameFrom, renameTo string
	copyFrom, copyTo     string
	similarity           string
	hasIndex             bool

	// minus and plus are the --- and +++ paths without their prefix, or
	// devNull; hasPatch reports that the lines were present.
	minus, plus string
	hasPatch    bool

	binary         bool
	added, removed []string

	// addedAt and removedAt parallel added and removed; newLine and
	// oldLine are the line numbers of the next line on each side.
	addedAt, removedAt []int
	newLine, oldLine   int
}

func (p *diffParser) file() (FileChange, error) {
	l, _ := p.next()
	rest, ok := strings.CutPrefix(l, "diff --git ")
	if !ok {
		return FileChange{}, errors.New("expected a diff --git header")
	}
	e := fileEntry{gitLine: rest}
	if err := p.extendedHeaders(&e); err != nil {
		return FileChange{}, err
	}
	if err := p.body(&e); err != nil {
		return FileChange{}, err
	}
	if !p.atFileEnd() {
		p.i++
		return FileChange{}, errors.New("unexpected line after file entry")
	}
	return e.resolve()
}

// extHeader is one extended header line git writes between diff --git
// and the patch body.
type extHeader struct {
	prefix string
	set    func(e *fileEntry, v string) error
}

var extHeaders = []extHeader{
	{"old mode ", func(e *fileEntry, v string) error { return setMode(&e.oldMode, v) }},
	{"new mode ", func(e *fileEntry, v string) error { return setMode(&e.newMode, v) }},
	{"new file mode ", func(e *fileEntry, v string) error { return setMode(&e.newFileMode, v) }},
	{"deleted file mode ", func(e *fileEntry, v string) error { return setMode(&e.delMode, v) }},
	{"rename from ", func(e *fileEntry, v string) error { return setPath(&e.renameFrom, v) }},
	{"rename to ", func(e *fileEntry, v string) error { return setPath(&e.renameTo, v) }},
	{"copy from ", func(e *fileEntry, v string) error { return setPath(&e.copyFrom, v) }},
	{"copy to ", func(e *fileEntry, v string) error { return setPath(&e.copyTo, v) }},
	{"similarity index ", func(e *fileEntry, v string) error { return setPercent(&e.similarity, v) }},
	{"index ", func(e *fileEntry, v string) error { return e.setIndex(v) }},
}

func (p *diffParser) extendedHeaders(e *fileEntry) error {
	for !p.atFileEnd() {
		l, _ := p.peek()
		h, ok := findExtHeader(l)
		if !ok {
			return nil
		}
		p.i++
		if err := h.set(e, strings.TrimPrefix(l, h.prefix)); err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(h.prefix), err)
		}
	}
	return nil
}

func findExtHeader(l string) (extHeader, bool) {
	for _, h := range extHeaders {
		if strings.HasPrefix(l, h.prefix) {
			return h, true
		}
	}
	return extHeader{}, false
}

func setMode(dst *string, v string) error {
	if *dst != "" {
		return errors.New("repeated")
	}
	if !gitModes[v] {
		return errors.New("not a git file mode")
	}
	*dst = v
	return nil
}

func setPath(dst *string, v string) error {
	if *dst != "" {
		return errors.New("repeated")
	}
	p, err := parsePathField(v)
	if err != nil {
		return err
	}
	*dst = p
	return nil
}

func setPercent(dst *string, v string) error {
	if *dst != "" {
		return errors.New("repeated")
	}
	if !percentLine.MatchString(v) {
		return errors.New("not a percentage")
	}
	*dst = v
	return nil
}

func (e *fileEntry) setIndex(v string) error {
	if e.hasIndex {
		return errors.New("repeated")
	}
	m := indexLine.FindStringSubmatch("index " + v)
	if m == nil {
		return errors.New("not an index line")
	}
	if m[3] != "" && !gitModes[m[3]] {
		return errors.New("not a git file mode")
	}
	e.hasIndex = true
	e.indexMode = m[3]
	return nil
}

// body reads what follows the extended headers: a text patch, a binary
// patch, a "Binary files differ" line, or nothing.
func (p *diffParser) body(e *fileEntry) error {
	if p.atFileEnd() {
		return nil
	}
	l, _ := p.next()
	switch {
	case strings.HasPrefix(l, "--- "):
		return p.textPatch(e, l)
	case l == "GIT binary patch":
		e.binary = true
		return p.binaryPatch()
	case strings.HasPrefix(l, "Binary files ") && strings.HasSuffix(l, " differ"):
		e.binary = true
		return nil
	}
	return errors.New("unexpected line in file header")
}

func (p *diffParser) textPatch(e *fileEntry, minusLine string) error {
	minus, err := patchPath(strings.TrimPrefix(minusLine, "--- "), "a/")
	if err != nil {
		return fmt.Errorf("--- line: %w", err)
	}
	l, _ := p.next()
	plusRest, ok := strings.CutPrefix(l, "+++ ")
	if !ok {
		return errors.New("expected a +++ line")
	}
	plus, err := patchPath(plusRest, "b/")
	if err != nil {
		return fmt.Errorf("+++ line: %w", err)
	}
	e.minus, e.plus, e.hasPatch = minus, plus, true
	for {
		if err := p.hunk(e); err != nil {
			return err
		}
		if l, ok := p.peek(); !ok || !strings.HasPrefix(l, "@@ ") {
			return nil
		}
	}
}

// hunk reads one @@ hunk and requires its lines to match the counts in
// its header exactly.
func (p *diffParser) hunk(e *fileEntry) error {
	l, _ := p.next()
	m := hunkHeader.FindStringSubmatch(l)
	if m == nil {
		return errors.New("expected a hunk header")
	}
	oldN, newN := hunkCount(m[2]), hunkCount(m[4])
	if err := e.startHunk(m[1], oldN, m[3], newN); err != nil {
		return err
	}
	switch {
	case oldN == 0 && newN == 0:
		return errors.New("empty hunk")
	case max(oldN, newN) > len(p.lines)-p.i:
		return errors.New("hunk longer than the diff")
	}
	prevContent := false
	for oldN > 0 || newN > 0 || p.peekNoNewline(prevContent) {
		l, ok := p.next()
		if !ok || l == "" {
			return errors.New("hunk shorter than its header")
		}
		var err error
		oldN, newN, err = e.hunkLine(l, oldN, newN, prevContent)
		if err != nil {
			return err
		}
		prevContent = l[0] != '\\'
	}
	return nil
}

func (p *diffParser) peekNoNewline(prevContent bool) bool {
	l, ok := p.peek()
	return ok && prevContent && l == noNewline
}

// startHunk positions a hunk from its header. A side with lines starts
// at line 1 or later, and each hunk starts no earlier than the previous
// one ended, as git writes them; anything else is refused, so line
// numbers stay strictly increasing. A side with no lines names the line
// before the gap, which may be 0.
func (e *fileEntry) startHunk(oldStart string, oldN int, newStart string, newN int) error {
	// The regular expression bounds each start to nine digits.
	o, _ := strconv.Atoi(oldStart)
	n, _ := strconv.Atoi(newStart)
	if (o == 0 && oldN > 0) || (n == 0 && newN > 0) {
		return errors.New("hunk starts at line 0")
	}
	if oldN == 0 {
		o++
	}
	if newN == 0 {
		n++
	}
	if o < e.oldLine || n < e.newLine {
		return errors.New("hunk overlaps or precedes the previous one")
	}
	e.oldLine, e.newLine = o, n
	return nil
}

// hunkLine accounts for one line of a hunk and returns the counts still
// expected.
func (e *fileEntry) hunkLine(l string, oldN, newN int, prevContent bool) (int, int, error) {
	switch l[0] {
	case ' ':
		if oldN == 0 || newN == 0 {
			return 0, 0, errors.New("hunk longer than its header")
		}
		e.oldLine++
		e.newLine++
		return oldN - 1, newN - 1, nil
	case '-':
		if oldN == 0 {
			return 0, 0, errors.New("hunk removes more lines than its header")
		}
		e.removed = append(e.removed, l[1:])
		e.removedAt = append(e.removedAt, e.oldLine)
		e.oldLine++
		return oldN - 1, newN, nil
	case '+':
		if newN == 0 {
			return 0, 0, errors.New("hunk adds more lines than its header")
		}
		e.added = append(e.added, l[1:])
		e.addedAt = append(e.addedAt, e.newLine)
		e.newLine++
		return oldN, newN - 1, nil
	case '\\':
		if l != noNewline || !prevContent {
			return 0, 0, errors.New("misplaced no-newline marker")
		}
		return oldN, newN, nil
	}
	return 0, 0, errors.New("unexpected line in hunk")
}

// hunkCount reads a hunk header's optional line count; absent means 1.
// The regular expression bounds it to nine digits.
func hunkCount(s string) int {
	if s == "" {
		return 1
	}
	n, _ := strconv.Atoi(s)
	return n
}

// binaryPatch reads one or two literal/delta sections: the forward
// change and, when git includes it, the reverse.
func (p *diffParser) binaryPatch() error {
	if err := p.binarySection(); err != nil {
		return err
	}
	if l, ok := p.peek(); ok && binarySize.MatchString(l) {
		return p.binarySection()
	}
	return nil
}

func (p *diffParser) binarySection() error {
	l, _ := p.next()
	if !binarySize.MatchString(l) {
		return errors.New("expected literal or delta")
	}
	n := 0
	for {
		l, ok := p.next()
		if !ok {
			return errors.New("binary patch not terminated")
		}
		if l == "" {
			break
		}
		if !validBase85Line(l) {
			return errors.New("invalid binary patch data")
		}
		n++
	}
	if n == 0 {
		return errors.New("empty binary patch section")
	}
	return nil
}

const base85Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!#$%&()*+-;<=>?@^_`{|}~"

// validBase85Line checks one line of git's binary patch encoding: a
// length character (A-Z for 1-26 bytes, a-z for 27-52) and five base85
// characters per four bytes.
func validBase85Line(l string) bool {
	var n int
	switch c := l[0]; {
	case c >= 'A' && c <= 'Z':
		n = int(c-'A') + 1
	case c >= 'a' && c <= 'z':
		n = int(c-'a') + 27
	default:
		return false
	}
	data := l[1:]
	if len(data) != (n+3)/4*5 {
		return false
	}
	for i := 0; i < len(data); i++ {
		if strings.IndexByte(base85Alphabet, data[i]) < 0 {
			return false
		}
	}
	return true
}

// resolve reconciles a file entry's headers into one FileChange. Paths
// come from the unambiguous lines (rename/copy, ---/+++) and must agree
// with the diff --git header wherever that header can be read.
func (e *fileEntry) resolve() (FileChange, error) {
	fc := FileChange{Binary: e.binary, Added: e.added, Removed: e.removed, AddedAt: e.addedAt, RemovedAt: e.removedAt}
	isRename := e.renameFrom != "" || e.renameTo != ""
	isCopy := e.copyFrom != "" || e.copyTo != ""
	if e.similarity != "" && !isRename && !isCopy {
		return FileChange{}, errors.New("similarity index without a rename or copy")
	}
	var err error
	switch {
	case isRename && isCopy:
		err = errors.New("both a rename and a copy")
	case isRename:
		err = e.resolveMove(&fc, ChangeRenamed, e.renameFrom, e.renameTo)
	case isCopy:
		err = e.resolveMove(&fc, ChangeCopied, e.copyFrom, e.copyTo)
	case e.newFileMode != "" && e.delMode != "":
		err = errors.New("both a new and a deleted file")
	case e.newFileMode != "":
		err = e.resolveAdded(&fc)
	case e.delMode != "":
		err = e.resolveDeleted(&fc)
	default:
		err = e.resolveModified(&fc)
	}
	if err != nil {
		return FileChange{}, err
	}
	return fc, nil
}

func (e *fileEntry) resolveMove(fc *FileChange, kind ChangeKind, from, to string) error {
	switch {
	case from == "" || to == "":
		return fmt.Errorf("%s without both paths", kind)
	case e.similarity == "":
		return fmt.Errorf("%s without a similarity index", kind)
	case e.newFileMode != "" || e.delMode != "":
		return fmt.Errorf("%s of a new or deleted file", kind)
	case e.hasPatch && (e.minus != from || e.plus != to):
		return fmt.Errorf("--- or +++ path disagrees with the %s", kind)
	}
	if !strings.HasPrefix(e.gitLine, "a/") && !strings.HasPrefix(e.gitLine, `"a/`) {
		return errors.New("diff --git header without a/ prefix")
	}
	fc.Kind, fc.OldPath, fc.NewPath = kind, from, to
	return e.resolveModes(fc)
}

func (e *fileEntry) resolveAdded(fc *FileChange) error {
	if e.oldMode != "" || e.newMode != "" || e.indexMode != "" {
		return errors.New("mode change on a new file")
	}
	if e.hasPatch && (e.minus != devNull || e.plus == devNull) {
		return errors.New("new file whose --- is not /dev/null")
	}
	p, err := e.headerPath(e.plus)
	if err != nil {
		return err
	}
	fc.Kind, fc.NewPath, fc.NewMode = ChangeAdded, p, e.newFileMode
	return nil
}

func (e *fileEntry) resolveDeleted(fc *FileChange) error {
	if e.oldMode != "" || e.newMode != "" || e.indexMode != "" {
		return errors.New("mode change on a deleted file")
	}
	if e.hasPatch && (e.plus != devNull || e.minus == devNull) {
		return errors.New("deleted file whose +++ is not /dev/null")
	}
	p, err := e.headerPath(e.minus)
	if err != nil {
		return err
	}
	fc.Kind, fc.OldPath, fc.OldMode = ChangeDeleted, p, e.delMode
	return nil
}

func (e *fileEntry) resolveModified(fc *FileChange) error {
	if e.hasPatch && (e.minus != e.plus || e.minus == devNull) {
		return errors.New("--- and +++ name different files without a rename")
	}
	p, err := e.headerPath(e.plus)
	if err != nil {
		return err
	}
	fc.Kind, fc.OldPath, fc.NewPath = ChangeModified, p, p
	if err := e.resolveModes(fc); err != nil {
		return err
	}
	if !e.hasPatch && !e.binary && e.oldMode == "" {
		return errors.New("file entry describes no change")
	}
	return nil
}

// headerPath reads the diff --git header of an entry that names one file
// and, when the entry had ---/+++ lines, requires it to agree with
// patchPath, the path they named.
func (e *fileEntry) headerPath(patchPath string) (string, error) {
	h, err := parseGitHeader(e.gitLine)
	if err != nil {
		return "", err
	}
	if e.hasPatch && patchPath != h {
		return "", errors.New("--- or +++ path disagrees with the diff --git header")
	}
	return h, nil
}

func (e *fileEntry) resolveModes(fc *FileChange) error {
	switch {
	case e.oldMode == "" && e.newMode == "":
		fc.OldMode, fc.NewMode = e.indexMode, e.indexMode
		return nil
	case e.oldMode == "" || e.newMode == "":
		return errors.New("old mode without new mode")
	case e.oldMode == e.newMode:
		return errors.New("mode change to the same mode")
	case e.indexMode != "":
		return errors.New("index mode beside a mode change")
	}
	fc.OldMode, fc.NewMode = e.oldMode, e.newMode
	return nil
}

// parseGitHeader reads the path from the rest of a diff --git line for an
// entry that names one file, "a/<p> b/<p>". Unquoted, the line is
// ambiguous only when the two paths differ, so equal halves are found by
// length; quoted, each half is read exactly.
func parseGitHeader(rest string) (string, error) {
	if strings.HasPrefix(rest, `"`) {
		return parseQuotedGitHeader(rest)
	}
	n := len(rest) - len("a/ b/")
	if n <= 0 || n%2 != 0 {
		return "", errors.New("unreadable diff --git header")
	}
	n /= 2
	a, sep, b := rest[:2+n], rest[2+n:5+n], rest[5+n:]
	if sep != " b/" || !strings.HasPrefix(a, "a/") || a[2:] != b {
		return "", errors.New("unreadable diff --git header")
	}
	if err := checkBareName(b); err != nil {
		return "", err
	}
	return b, nil
}

func parseQuotedGitHeader(rest string) (string, error) {
	a, r, err := unquoteC(rest)
	if err != nil {
		return "", err
	}
	r, ok := strings.CutPrefix(r, " ")
	if !ok || !strings.HasPrefix(r, `"`) {
		return "", errors.New("unreadable diff --git header")
	}
	b, r, err := unquoteC(r)
	if err != nil {
		return "", err
	}
	a, okA := strings.CutPrefix(a, "a/")
	b, okB := strings.CutPrefix(b, "b/")
	if r != "" || !okA || !okB || a != b || a == "" {
		return "", errors.New("unreadable diff --git header")
	}
	return a, nil
}

// patchPath reads the path on a --- or +++ line: /dev/null or prefix
// followed by the path, quoted when git quotes it. Git appends a tab when
// the path contains a space.
func patchPath(v, prefix string) (string, error) {
	body, tab := strings.CutSuffix(v, "\t")
	if body == devNull {
		return devNull, nil
	}
	name, err := quotedOrBare(body)
	if err != nil {
		return "", err
	}
	if tab != strings.Contains(name, " ") {
		return "", errors.New("path tab marker does not match")
	}
	p, ok := strings.CutPrefix(name, prefix)
	if !ok || p == "" {
		return "", fmt.Errorf("path without %s prefix", prefix)
	}
	return p, nil
}

// parsePathField reads the path in a rename or copy line, which has no
// prefix.
func parsePathField(v string) (string, error) {
	return quotedOrBare(v)
}

// quotedOrBare reads a whole field as one path, quoted or not.
func quotedOrBare(v string) (string, error) {
	if !strings.HasPrefix(v, `"`) {
		if err := checkBareName(v); err != nil {
			return "", err
		}
		return v, nil
	}
	p, r, err := unquoteC(v)
	switch {
	case err != nil:
		return "", err
	case r != "":
		return "", errors.New("text after quoted path")
	case p == "":
		return "", errors.New("empty path")
	}
	return p, nil
}

// checkBareName refuses an unquoted path git would have quoted: one with
// a quote, a backslash, a control character or a byte above 0x7e.
func checkBareName(s string) error {
	if s == "" {
		return errors.New("empty path")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			return errors.New("unquoted path with a character git quotes")
		}
	}
	return nil
}

// unquoteC decodes the C-style quoted string at the start of s, as git's
// quote_c_style writes it, and returns it with the text after the
// closing quote.
func unquoteC(s string) (string, string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			return b.String(), s[i+1:], nil
		case c < 0x20 || c >= 0x7f:
			return "", "", errors.New("unescaped byte in quoted path")
		case c != '\\':
			b.WriteByte(c)
			continue
		}
		d, n, err := cEscape(s[i+1:])
		if err != nil {
			return "", "", err
		}
		b.WriteByte(d)
		i += n
	}
	return "", "", errors.New("unterminated quoted path")
}

var cEscapes = map[byte]byte{'a': '\a', 'b': '\b', 't': '\t', 'n': '\n', 'v': '\v', 'f': '\f', 'r': '\r', '"': '"', '\\': '\\'}

// cEscape decodes the escape after a backslash: one letter or three octal
// digits. It returns the byte and how many characters it consumed.
func cEscape(s string) (byte, int, error) {
	if s == "" {
		return 0, 0, errors.New("unterminated escape in quoted path")
	}
	if d, ok := cEscapes[s[0]]; ok {
		return d, 1, nil
	}
	if len(s) < 3 || s[0] < '0' || s[0] > '3' || !isOctal(s[1]) || !isOctal(s[2]) {
		return 0, 0, errors.New("invalid escape in quoted path")
	}
	return (s[0]-'0')<<6 | (s[1]-'0')<<3 | (s[2] - '0'), 3, nil
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
