package gate

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// maxJUnitBytes bounds the JUnit XML the tests gate will parse. A report
// is a few kilobytes per hundred tests; anything this large is refused.
const maxJUnitBytes = 16 << 20

// maxDeclaredCount bounds one failures or errors attribute, so summing
// them cannot overflow.
const maxDeclaredCount = 1 << 30

// errNoReport means the test command left no JUnit report to read.
var errNoReport = errors.New("no JUnit report")

// junitCounts is what the tests gate learns from a JUnit report.
type junitCounts struct {
	// Cases counts <testcase> elements. Failed counts those with a
	// <failure> or <error> child, Skipped those with a <skipped> child
	// and neither of the others.
	Cases, Failed, Skipped int

	// DeclaredFailed sums the failures and errors attributes of every
	// <testsuite> and <testsuites>.
	DeclaredFailed int
}

// Ran is the number of test cases that executed.
func (c junitCounts) Ran() int { return c.Cases - c.Skipped }

// Passed reports whether the report shows at least one test ran and
// nothing failed, by either count.
func (c junitCounts) Passed() bool {
	return c.Ran() > 0 && c.Failed == 0 && c.DeclaredFailed == 0
}

// parseJUnit reads one or more JUnit XML documents, concatenated, each
// rooted at <testsuites> or <testsuite>. The JUnit console launcher writes
// one file per engine (TEST-junit-jupiter.xml, TEST-junit-vintage.xml),
// and the gate reads them all in one stream.
//
// It counts <testcase> elements and their children rather than trusting
// the summary attributes, and also sums the suites' declared failures and
// errors. Either count showing a failure fails the gate: the two can only
// disagree if something rewrote the report, and that is no reason to pass.
// Empty input, or input with no root element, is errNoReport; malformed
// XML, an unexpected root element, or input past maxJUnitBytes is an
// error.
func parseJUnit(report []byte) (junitCounts, error) {
	if len(report) > maxJUnitBytes {
		return junitCounts{}, fmt.Errorf("JUnit report is %d bytes, cap %d", len(report), maxJUnitBytes)
	}
	p := junitParser{dec: xml.NewDecoder(bytes.NewReader(report))}
	if err := p.run(); err != nil {
		return junitCounts{}, err
	}
	if p.roots == 0 {
		return junitCounts{}, errNoReport
	}
	return p.counts, nil
}

// junitParser walks the token stream once. depth is the element depth;
// caseDepth is the depth of the open <testcase>, or 0.
type junitParser struct {
	dec       *xml.Decoder
	counts    junitCounts
	roots     int
	depth     int
	caseDepth int
	// caseFailed and caseSkipped describe the open <testcase>.
	caseFailed, caseSkipped bool
}

func (p *junitParser) run() error {
	for {
		tok, err := p.dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("parsing JUnit report: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := p.start(t); err != nil {
				return err
			}
		case xml.EndElement:
			p.end()
		}
	}
}

func (p *junitParser) start(t xml.StartElement) error {
	p.depth++
	name := t.Name.Local
	if p.depth == 1 {
		if name != "testsuites" && name != "testsuite" {
			return fmt.Errorf("parsing JUnit report: unexpected root element <%s>", name)
		}
		p.roots++
	}
	switch {
	case name == "testsuites" || name == "testsuite":
		n, err := declaredFailures(t)
		if err != nil {
			return err
		}
		p.counts.DeclaredFailed += n
	case name == "testcase" && p.caseDepth == 0:
		p.caseDepth = p.depth
		p.caseFailed, p.caseSkipped = false, false
	case p.caseDepth != 0 && p.depth == p.caseDepth+1:
		p.caseFailed = p.caseFailed || name == "failure" || name == "error"
		p.caseSkipped = p.caseSkipped || name == "skipped"
	}
	return nil
}

func (p *junitParser) end() {
	if p.caseDepth != 0 && p.depth == p.caseDepth {
		p.counts.Cases++
		switch {
		case p.caseFailed:
			p.counts.Failed++
		case p.caseSkipped:
			p.counts.Skipped++
		}
		p.caseDepth = 0
	}
	p.depth--
}

// declaredFailures sums a suite's failures and errors attributes. A
// missing attribute counts as zero; one that is not a non-negative
// integer makes the report unreadable.
func declaredFailures(t xml.StartElement) (int, error) {
	total := 0
	for _, a := range t.Attr {
		if a.Name.Local != "failures" && a.Name.Local != "errors" {
			continue
		}
		n, err := strconv.Atoi(a.Value)
		if err != nil || n < 0 || n > maxDeclaredCount {
			return 0, fmt.Errorf("parsing JUnit report: <%s %s=%q> is not a count", t.Name.Local, a.Name.Local, a.Value)
		}
		total += n
	}
	return total, nil
}
