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
		hits = append(hits, weakeningHits(c, pats, p.Tests.SkipMarkers)...)
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
func weakeningHits(c FileChange, pats *policyPatterns, skips []*regexp.Regexp) []string {
	var hits []string
	oldTest := c.OldPath != "" && matchesAny(pats.tests, c.OldPath)
	newTest := c.NewPath != "" && matchesAny(pats.tests, c.NewPath)
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
	if newTest && containsMarker(c.Added, skips) {
		hits = append(hits, quotePath(c.NewPath)+" (skip marker added)")
	}
	for _, path := range changePaths(c) {
		if matchesAny(pats.testConfig, path) {
			hits = append(hits, quotePath(path)+" (test configuration changed)")
		}
	}
	return hits
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
