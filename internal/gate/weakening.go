package gate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
)

// checkWeakening fails a diff that makes the tests easier to pass without
// doing the work (FR-A3), as the repository's TestPolicy describes tests:
//   - a test file is deleted, or renamed out of the test paths;
//   - test case markers are removed more often than added, counted over
//     every test file (a marker counts only at a line's first non-blank
//     character, so a commented-out case counts as removed);
//   - an added line in a test file contains a skip marker;
//   - a run of adjacent lines added to or removed from an existing test
//     file leaves a block comment open, or closes one opened elsewhere,
//     so a comment boundary moved;
//   - anything under the test configuration paths changes;
//   - an existing test file changes as binary, which hides its lines.
//
// The tests gate's JUnit report check is the runtime half: a test that
// still exists must still run.
func checkWeakening(changes []FileChange, p Policy) verdict {
	v := verdict{Gate: contracts.GateWeakening}
	pats, err := p.patterns()
	if err != nil || len(pats.tests) == 0 {
		v.Detail = "test policy not configured"
		return v
	}
	var hits []string
	removed, added := 0, 0
	for _, c := range changes {
		hits = append(hits, weakeningHits(c, pats, p.Tests)...)
		if c.OldPath != "" && matchesAny(pats.tests, c.OldPath) {
			removed += countCaseMarkers(c.Removed, p.Tests.CaseMarkers)
		}
		if c.NewPath != "" && matchesAny(pats.tests, c.NewPath) {
			added += countCaseMarkers(c.Added, p.Tests.CaseMarkers)
		}
	}
	if removed > added {
		hits = append(hits, fmt.Sprintf("%d test cases removed, %d added", removed, added))
	}
	if len(hits) > 0 {
		v.Detail = "tests weakened: " + listSome(hits)
		return v
	}
	v.Passed = true
	v.Detail = "no tests deleted, skipped or reconfigured"
	return v
}

// weakeningHits describes what one change does to the tests, apart from
// the case-marker count, which is taken over the whole diff.
func weakeningHits(c FileChange, pats *policyPatterns, tp TestPolicy) []string {
	oldTest := c.OldPath != "" && matchesAny(pats.tests, c.OldPath)
	newTest := c.NewPath != "" && matchesAny(pats.tests, c.NewPath)
	hits := append(testFileHits(c, oldTest, newTest), markerHits(c, oldTest, newTest, tp)...)
	for _, path := range changePaths(c) {
		if matchesAny(pats.testConfig, path) {
			hits = append(hits, quotePath(path)+" (test configuration changed)")
		}
	}
	return hits
}

// testFileHits reports a test file deleted, renamed out of the test
// paths, or changed as binary.
func testFileHits(c FileChange, oldTest, newTest bool) []string {
	var hits []string
	switch {
	case oldTest && c.Kind == ChangeDeleted:
		hits = append(hits, quotePath(c.OldPath)+" (test deleted)")
	case oldTest && c.Kind == ChangeRenamed && !newTest:
		hits = append(hits, quotePath(c.OldPath)+" (test renamed out of the test paths)")
	}
	if oldTest && c.Binary && c.Kind != ChangeDeleted {
		// Binary content hides its lines from the marker checks.
		hits = append(hits, quotePath(c.OldPath)+" (test changed as binary)")
	}
	return hits
}

// markerHits reports a skip marker added to a test file, and a block
// comment boundary moved in an existing one. A new file is not checked
// for comments: it has no existing cases to hide.
func markerHits(c FileChange, oldTest, newTest bool, tp TestPolicy) []string {
	var hits []string
	if newTest && containsMarker(c.Added, tp.SkipMarkers) {
		hits = append(hits, quotePath(c.NewPath)+" (skip marker added)")
	}
	if c.Kind == ChangeAdded {
		return hits
	}
	moved := (newTest && !commentsBalanced(c.Added, c.AddedAt, tp)) ||
		(oldTest && !commentsBalanced(c.Removed, c.RemovedAt, tp))
	if moved {
		hits = append(hits, quotePath(changePath(c))+" (block comment boundary moved)")
	}
	return hits
}

// commentsBalanced reports whether every run of adjacent lines (by line
// number in at) opens and closes its own block comments: no line closes
// a comment the run did not open, and the run does not end inside one.
// A line may close a comment and then open another, in that order. A
// line number list that does not parallel lines is not balanced.
func commentsBalanced(lines []string, at []int, tp TestPolicy) bool {
	if len(at) != len(lines) {
		return false
	}
	open := false
	for i, l := range lines {
		if open && i > 0 && at[i] != at[i-1]+1 {
			// The comment runs on into lines the change did not touch.
			return false
		}
		if containsMarker([]string{l}, tp.CommentClosers) {
			if !open {
				return false
			}
			open = false
		}
		if containsMarker([]string{l}, tp.CommentOpeners) {
			open = true
		}
	}
	return !open
}

func matchesAny(ps []pattern, path string) bool {
	_, ok := matchAny(ps, path)
	return ok
}

// countCaseMarkers counts the lines that begin, after indentation, with a
// case marker.
func countCaseMarkers(lines []string, markers []*regexp.Regexp) int {
	n := 0
	for _, l := range lines {
		l = strings.TrimLeft(l, " \t")
		for _, re := range markers {
			if loc := re.FindStringIndex(l); loc != nil && loc[0] == 0 {
				n++
				break
			}
		}
	}
	return n
}

// containsMarker reports whether any line contains any marker.
func containsMarker(lines []string, markers []*regexp.Regexp) bool {
	for _, l := range lines {
		for _, re := range markers {
			if re.MatchString(l) {
				return true
			}
		}
	}
	return false
}
