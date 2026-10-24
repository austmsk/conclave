package gate

import (
	"errors"
	"strings"
	"testing"
)

// jupiterReport is the shape the JUnit console launcher writes with
// --reports-dir: one file per engine, each with an XML declaration.
const jupiterReport = `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="JUnit Jupiter" tests="3" skipped="1" failures="0" errors="0" time="0.1">
<properties><property name="java.version" value="21"/></properties>
<testcase name="countsVotes()" classname="TallyTest" time="0.01"><system-out><![CDATA[unique-id: [engine:junit-jupiter]]]></system-out></testcase>
<testcase name="rejectsBallot()" classname="TallyTest" time="0.01"/>
<testcase name="pending()" classname="TallyTest" time="0"><skipped/></testcase>
</testsuite>
`

const vintageReport = `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="JUnit Vintage" tests="0" skipped="0" failures="0" errors="0" time="0">
</testsuite>
`

func TestParseJUnit(t *testing.T) {
	tests := []struct {
		name    string
		report  string
		want    junitCounts
		wantErr error // errNoReport, or nil
		anyErr  bool
	}{
		{name: "console launcher pair", report: jupiterReport + vintageReport, want: junitCounts{Cases: 3, Skipped: 1}},
		{name: "testsuites root", report: `<testsuites><testsuite><testcase name="a"/></testsuite><testsuite><testcase name="b"/></testsuite></testsuites>`, want: junitCounts{Cases: 2}},
		{name: "bare testsuite", report: `<testsuite><testcase name="a"/></testsuite>`, want: junitCounts{Cases: 1}},
		{name: "failure child", report: `<testsuite failures="1"><testcase name="a"><failure message="x"/></testcase><testcase name="b"/></testsuite>`, want: junitCounts{Cases: 2, Failed: 1, DeclaredFailed: 1}},
		{name: "error child", report: `<testsuite><testcase name="a"><error/></testcase></testsuite>`, want: junitCounts{Cases: 1, Failed: 1}},
		{name: "failed and skipped counts as failed", report: `<testsuite><testcase><skipped/><failure/></testcase></testsuite>`, want: junitCounts{Cases: 1, Failed: 1}},
		{name: "declared failures without a failing case", report: `<testsuites failures="2"><testsuite errors="1"><testcase/></testsuite></testsuites>`, want: junitCounts{Cases: 1, DeclaredFailed: 3}},
		{name: "failure nested deeper is not the case's", report: `<testsuite><testcase><system-out><failure/></system-out></testcase></testsuite>`, want: junitCounts{Cases: 1}},
		{name: "nested suites", report: `<testsuites><testsuite><testsuite><testcase/><testcase><skipped/></testcase></testsuite></testsuite></testsuites>`, want: junitCounts{Cases: 2, Skipped: 1}},
		{name: "namespaced elements count by local name", report: `<j:testsuite xmlns:j="urn:x"><j:testcase><j:failure/></j:testcase></j:testsuite>`, want: junitCounts{Cases: 1, Failed: 1}},
		{name: "summary attributes are not trusted for passes", report: `<testsuite tests="50" failures="0"></testsuite>`, want: junitCounts{}},
		{name: "empty", report: "", wantErr: errNoReport},
		{name: "whitespace and declaration only", report: "<?xml version=\"1.0\"?>\n  \n", wantErr: errNoReport},
		{name: "unexpected root", report: `<html><testcase/></html>`, anyErr: true},
		{name: "second document with unexpected root", report: jupiterReport + `<results/>`, anyErr: true},
		{name: "malformed", report: `<testsuite><testcase></testsuite>`, anyErr: true},
		{name: "truncated", report: `<testsuite><testcase/>`, anyErr: true},
		{name: "non-numeric failures", report: `<testsuite failures="none"><testcase/></testsuite>`, anyErr: true},
		{name: "negative errors", report: `<testsuite errors="-1"><testcase/></testsuite>`, anyErr: true},
		{name: "huge failures", report: `<testsuite failures="99999999999"><testcase/></testsuite>`, anyErr: true},
		{name: "oversized", report: "<testsuite>" + strings.Repeat(" ", maxJUnitBytes) + "</testsuite>", anyErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseJUnit([]byte(tt.report))
			switch {
			case tt.anyErr:
				if err == nil {
					t.Fatalf("parseJUnit = %+v, want an error", got)
				}
			case !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil):
				t.Fatalf("parseJUnit error = %v, want %v", err, tt.wantErr)
			case got != tt.want:
				t.Errorf("parseJUnit = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestJUnitCountsPassed(t *testing.T) {
	tests := []struct {
		name   string
		counts junitCounts
		want   bool
	}{
		{name: "tests ran, none failed", counts: junitCounts{Cases: 3, Skipped: 1}, want: true},
		{name: "no tests", counts: junitCounts{}},
		{name: "all skipped", counts: junitCounts{Cases: 2, Skipped: 2}},
		{name: "a failure", counts: junitCounts{Cases: 2, Failed: 1}},
		{name: "declared failure only", counts: junitCounts{Cases: 2, DeclaredFailed: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.counts.Passed(); got != tt.want {
				t.Errorf("Passed() = %t, want %t", got, tt.want)
			}
		})
	}
}
