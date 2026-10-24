package gate

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// Finding is one secret-scanner match. It never holds the matched text.
type Finding struct {
	RuleID string
	Path   string

	// Line is the ordinal of the added line within Path's added lines,
	// from 1, in diff order. FileChange carries no hunk positions, so this
	// is not a line number in the new file. Zero means the match is in the
	// header that names the path: the path itself looks like a secret.
	Line int
}

// SecretScanner scans text for secrets. It is an external-process
// boundary: the production implementation runs a pinned gitleaks binary.
// An error is an environment failure, never a verdict.
//
// Scan reports each finding's line in input, from 1, in Line, and leaves
// Path empty: input is one stream, and checkSecrets maps its lines back to
// files.
type SecretScanner interface {
	Scan(ctx context.Context, input []byte) ([]Finding, error)
}

// maxFindingsInDetail caps how many findings a verdict lists, so one diff
// full of keys cannot produce an unbounded Detail.
const maxFindingsInDetail = 10

// fileHeaderPrefix opens the line naming each file in the scan input. The
// path is Go-quoted, so a path holding a newline or other control byte
// cannot add a line and shift every line after it.
const fileHeaderPrefix = "# conclave-file: "

// lineRef locates one line of the scan input in the diff: a file and the
// ordinal of the added line within it, zero for the file's header.
type lineRef struct {
	path string
	line int
}

// scanInput is the text handed to the scanner and the map from its line
// numbers back to the diff.
type scanInput struct {
	text []byte

	// lines[i] locates input line i+1, the numbering gitleaks reports.
	lines []lineRef

	// added counts the added lines in text, headers excluded.
	added int

	// binaryPaths are files with added or changed binary content, which
	// no text scanner can read.
	binaryPaths []string
}

// secretsInput builds the scan input from the added lines of every file:
//
//	# conclave-file: "src/a.go"
//	<first added line of src/a.go>
//	<second added line>
//	# conclave-file: "src/b.go"
//	...
//
// Removed lines are not scanned: a secret leaving the tree is not a secret
// being pushed. Line numbers in the scanner's report index into the input,
// and the returned map, built here rather than parsed back out of the
// input, turns each into a path and an added-line ordinal. An added line
// that imitates a header therefore cannot redirect a finding.
func secretsInput(changes []FileChange) (input []byte, binaryPaths []string) {
	in := buildScanInput(changes)
	return in.text, in.binaryPaths
}

func buildScanInput(changes []FileChange) scanInput {
	var in scanInput
	var b bytes.Buffer
	for _, c := range changes {
		path := changePath(c)
		if c.Binary && c.Kind != ChangeDeleted {
			in.binaryPaths = append(in.binaryPaths, path)
		}
		if len(c.Added) == 0 {
			continue
		}
		b.WriteString(fileHeaderPrefix + strconv.Quote(path) + "\n")
		in.lines = append(in.lines, lineRef{path: path})
		for i, l := range c.Added {
			// A lone \n inside a line would desynchronise the map; the
			// parser splits on \n, so this only guards hand-built input.
			b.WriteString(strings.ReplaceAll(l, "\n", " ") + "\n")
			in.lines = append(in.lines, lineRef{path: path, line: i + 1})
		}
		in.added += len(c.Added)
	}
	in.text = b.Bytes()
	return in
}

// changePath names a change by its new path, or its old one when deleted.
func changePath(c FileChange) string {
	if c.NewPath != "" {
		return c.NewPath
	}
	return c.OldPath
}

// checkSecrets runs the secrets gate over the added lines. Binary content
// fails the gate outright, since no scanner reads it; the text is still
// scanned, so a scanner failure is reported even then. Findings fail the
// gate with their rule IDs and locations only, never the matched text. A
// scanner error, or a finding that points outside the input, is an
// environment failure.
func checkSecrets(ctx context.Context, s SecretScanner, changes []FileChange) (verdict, error) {
	start := time.Now()
	in := buildScanInput(changes)
	var findings []Finding
	if in.added > 0 {
		raw, err := s.Scan(ctx, in.text)
		if err != nil {
			return verdict{}, fmt.Errorf("%w: scanning added lines for secrets: %w", ErrEnvironment, err)
		}
		findings, err = locate(raw, in.lines)
		if err != nil {
			return verdict{}, err
		}
	}
	v := verdict{Gate: contracts.GateSecrets, Passed: true, Duration: time.Since(start)}
	var parts []string
	if len(in.binaryPaths) > 0 {
		v.Passed = false
		parts = append(parts, "binary content not scanned: "+listCapped(in.binaryPaths, quotePath))
	}
	if len(findings) > 0 {
		v.Passed = false
		parts = append(parts, fmt.Sprintf("%d secret finding(s): %s", len(findings), listCapped(findings, Finding.String)))
	}
	v.Detail = strings.Join(parts, "; ")
	return v, nil
}

// locate maps each finding's input line to its file and added-line
// ordinal. The scanner reports input line numbers in Line and leaves Path
// empty; a line outside the input means the scanner and the input
// disagree, which no verdict can rest on.
func locate(raw []Finding, lines []lineRef) ([]Finding, error) {
	out := make([]Finding, 0, len(raw))
	for _, f := range raw {
		if f.Line < 1 || f.Line > len(lines) {
			return nil, environmentf("secret scanner reported input line %d of %d", f.Line, len(lines))
		}
		ref := lines[f.Line-1]
		out = append(out, Finding{RuleID: f.RuleID, Path: ref.path, Line: ref.line})
	}
	return out, nil
}

// String renders a finding for Detail: rule ID and location, nothing the
// scanner matched.
func (f Finding) String() string {
	if f.Line == 0 {
		return fmt.Sprintf("%s in path %s", f.RuleID, quotePath(f.Path))
	}
	return fmt.Sprintf("%s at %s added line %d", f.RuleID, quotePath(f.Path), f.Line)
}

// quotePath Go-quotes a path, so control and bidi characters in an
// attacker-chosen name cannot disguise the evidence.
func quotePath(p string) string {
	return strconv.QuoteToASCII(p)
}

// listCapped joins the first maxFindingsInDetail items and says how many
// more there were.
func listCapped[T any](items []T, render func(T) string) string {
	n := min(len(items), maxFindingsInDetail)
	parts := make([]string, 0, n+1)
	for _, it := range items[:n] {
		parts = append(parts, render(it))
	}
	if len(items) > n {
		parts = append(parts, fmt.Sprintf("and %d more", len(items)-n))
	}
	return strings.Join(parts, ", ")
}
