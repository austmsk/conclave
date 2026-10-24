package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

var testCommands = Commands{
	Build:   []string{"make", "build"},
	Lint:    []string{"make", "lint"},
	Test:    []string{"make", "test"},
	Timeout: time.Minute,
}

// passingReport is a JUnit report with two tests run and none failed.
const passingReport = `<testsuite tests="2"><testcase name="a"/><testcase name="b"/></testsuite>`

// reply is one scripted Exec answer.
type reply struct {
	res contracts.ExecResult
	err error
}

// commandSandbox scripts a fakeSandbox through the command gates.
// replies is keyed by the joined command; anything unlisted exits 0 with
// output that names the command. report answers the JUnit read, clear
// the deletion of the report directory.
type commandSandbox struct {
	replies map[string]reply
	report  reply
	clear   reply
}

func (s commandSandbox) exec(req contracts.ExecRequest) (contracts.ExecResult, error) {
	cmd := strings.Join(req.Command, " ")
	switch {
	case strings.HasPrefix(cmd, "rm -rf -- "+JUnitDir):
		return s.clear.res, s.clear.err
	case strings.HasPrefix(cmd, "find "+JUnitDir):
		return s.report.res, s.report.err
	}
	if r, ok := s.replies[cmd]; ok {
		return r.res, r.err
	}
	return contracts.ExecResult{Stdout: []byte("canary_output_of " + cmd), Stderr: []byte("canary_stderr")}, nil
}

func withReport(report string) commandSandbox {
	return commandSandbox{report: reply{res: contracts.ExecResult{Stdout: []byte(report)}}}
}

// TestRunCommands_AllPass: three passing verdicts in order, each with its
// output stored, the tests verdict counting the report.
func TestRunCommands_AllPass(t *testing.T) {
	blobs := newTestBlobs(t)
	sb := newFakeSandbox("gating")
	sb.exec = withReport(passingReport).exec
	got, err := runCommands(context.Background(), sb, blobs, testRepoID, testCommands)
	if err != nil {
		t.Fatal(err)
	}
	want := []verdict{
		{Gate: contracts.GateBuild, Passed: true, Detail: "exit 0"},
		{Gate: contracts.GateLint, Passed: true, Detail: "exit 0"},
		{Gate: contracts.GateTests, Passed: true, Detail: "2 tests ran, 0 failed, 0 skipped"},
	}
	assertVerdicts(t, got, want)
	for _, v := range got {
		out, err := blobs.Get(context.Background(), v.Key)
		if err != nil {
			t.Fatalf("%s output: %v", v.Gate, err)
		}
		if !strings.Contains(string(out), "canary_output_of make") || !strings.Contains(string(out), "canary_stderr") {
			t.Errorf("%s output blob lacks the command's output: %q", v.Gate, out)
		}
	}
	tests, _ := blobs.Get(context.Background(), got[2].Key)
	if !strings.Contains(string(tests), passingReport) {
		t.Errorf("tests output blob lacks the JUnit report: %q", tests)
	}
	clear, run, read := sb.ran("rm -rf -- "+JUnitDir), sb.ran("make test"), sb.ran("find "+JUnitDir)
	if clear < 0 || clear >= run || run >= read {
		t.Errorf("want clear < run < read, got %d, %d, %d: %q", clear, run, read, sb.commands())
	}
	for _, req := range sb.execs {
		if strings.HasPrefix(req.Command[0], "make") && (req.WorkDir != "" || req.Timeout != time.Minute) {
			t.Errorf("%q ran in %q with timeout %s", req.Command, req.WorkDir, req.Timeout)
		}
	}
}

// assertVerdicts compares gate, pass and detail, and checks that every
// gate that ran stored its output and no detail quotes the sandbox.
func assertVerdicts(t *testing.T, got, want []verdict) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d verdicts %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		g := got[i]
		if g.Gate != want[i].Gate || g.Passed != want[i].Passed || g.Detail != want[i].Detail {
			t.Errorf("verdict %d = {%s %t %q}, want {%s %t %q}", i, g.Gate, g.Passed, g.Detail, want[i].Gate, want[i].Passed, want[i].Detail)
		}
		if strings.HasPrefix(g.Detail, "not run") != (g.Key == "") {
			t.Errorf("verdict %d: detail %q with key %q", i, g.Detail, g.Key)
		}
		if strings.Contains(g.Detail, "canary") {
			t.Errorf("verdict %d detail quotes sandbox output: %q", i, g.Detail)
		}
	}
}

// TestRunCommands_Failures: each way a command gate fails through the
// attempt's own fault, and that nothing after the first failure runs.
func TestRunCommands_Failures(t *testing.T) {
	notRun := func(g contracts.GateName) verdict { return verdict{Gate: g, Detail: detailEarlierFailed} }
	pass := func(g contracts.GateName) verdict { return verdict{Gate: g, Passed: true, Detail: "exit 0"} }
	fail := func(g contracts.GateName, d string) verdict { return verdict{Gate: g, Detail: d} }
	exit := func(code int) reply {
		return reply{res: contracts.ExecResult{ExitCode: code, Stdout: []byte("canary_failure")}}
	}
	tests := []struct {
		name    string
		script  commandSandbox
		want    []verdict
		notRuns []string
	}{
		{name: "build exits 1", script: commandSandbox{replies: map[string]reply{"make build": exit(1)}},
			want:    []verdict{fail(contracts.GateBuild, "exit 1"), notRun(contracts.GateLint), notRun(contracts.GateTests)},
			notRuns: []string{"make lint", "make test", "find"}},
		{name: "lint times out", script: commandSandbox{replies: map[string]reply{"make lint": {res: contracts.ExecResult{TimedOut: true, ExitCode: -1, Duration: time.Minute}}}},
			want:    []verdict{pass(contracts.GateBuild), fail(contracts.GateLint, "timed out after 1m0s"), notRun(contracts.GateTests)},
			notRuns: []string{"make test"}},
		{name: "build output past limit", script: commandSandbox{replies: map[string]reply{"make build": {err: fmt.Errorf("exec: %w", sandbox.ErrOutputTooLarge)}}},
			want: []verdict{fail(contracts.GateBuild, "output exceeded limit"), notRun(contracts.GateLint), notRun(contracts.GateTests)}},
		{name: "tests exit 1 with a passing report", script: func() commandSandbox {
			s := withReport(passingReport)
			s.replies = map[string]reply{"make test": exit(1)}
			return s
		}(),
			want:    []verdict{pass(contracts.GateBuild), pass(contracts.GateLint), fail(contracts.GateTests, "exit 1")},
			notRuns: []string{"find"}},
		{name: "tests exit 0, no report directory", script: commandSandbox{report: exit(1)},
			want: []verdict{pass(contracts.GateBuild), pass(contracts.GateLint), fail(contracts.GateTests, "exit 0 but no JUnit report")}},
		{name: "tests exit 0, no report files", script: withReport(""),
			want: []verdict{pass(contracts.GateBuild), pass(contracts.GateLint), fail(contracts.GateTests, "exit 0 but no JUnit report")}},
		{name: "report shows no tests", script: withReport(`<testsuite tests="0"></testsuite>`),
			want: []verdict{pass(contracts.GateBuild), pass(contracts.GateLint), fail(contracts.GateTests, "JUnit report shows no tests ran (0 skipped)")}},
		{name: "report shows all skipped", script: withReport(`<testsuite><testcase><skipped/></testcase></testsuite>`),
			want: []verdict{pass(contracts.GateBuild), pass(contracts.GateLint), fail(contracts.GateTests, "JUnit report shows no tests ran (1 skipped)")}},
		{name: "report shows a failure", script: withReport(`<testsuite><testcase/><testcase><failure/></testcase></testsuite>`),
			want: []verdict{pass(contracts.GateBuild), pass(contracts.GateLint), fail(contracts.GateTests, "1 of 2 tests failed or errored")}},
		{name: "report declares an error", script: withReport(`<testsuite errors="1"><testcase/></testsuite>`),
			want: []verdict{pass(contracts.GateBuild), pass(contracts.GateLint), fail(contracts.GateTests, "JUnit report declares 1 failures or errors")}},
		{name: "report malformed", script: withReport(`<testsuite><testcase>`),
			want: []verdict{pass(contracts.GateBuild), pass(contracts.GateLint), fail(contracts.GateTests, "JUnit report unreadable")}},
		{name: "report past output limit", script: commandSandbox{report: reply{err: sandbox.ErrOutputTooLarge}},
			want: []verdict{pass(contracts.GateBuild), pass(contracts.GateLint), fail(contracts.GateTests, "JUnit report exceeded output limit")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := newFakeSandbox("gating")
			sb.exec = tt.script.exec
			got, err := runCommands(context.Background(), sb, newTestBlobs(t), testRepoID, testCommands)
			if err != nil {
				t.Fatalf("runCommands error: %v", err)
			}
			assertVerdicts(t, got, tt.want)
			for _, cmd := range tt.notRuns {
				if sb.ran(cmd) >= 0 {
					t.Errorf("%q ran after a failure: %q", cmd, sb.commands())
				}
			}
		})
	}
}

// failingBlobs is a blob store that cannot store anything.
type failingBlobs struct{}

func (failingBlobs) Put(context.Context, string, []byte) (string, error) {
	return "", errors.New("disk full")
}

func (failingBlobs) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("disk full")
}

// TestRunCommands_Environment: when a gate cannot be decided, runCommands
// returns ErrEnvironment and no verdicts at all, so nothing is recorded.
func TestRunCommands_Environment(t *testing.T) {
	tests := []struct {
		name   string
		script commandSandbox
		cmds   Commands
		blobs  bool // use failingBlobs
	}{
		{name: "build not found", script: commandSandbox{replies: map[string]reply{"make build": {res: contracts.ExecResult{ExitCode: 127}}}}},
		{name: "lint not executable", script: commandSandbox{replies: map[string]reply{"make lint": {res: contracts.ExecResult{ExitCode: 126}}}}},
		{name: "tests: sandbox killed", script: commandSandbox{replies: map[string]reply{"make test": {err: errors.New("exec \"make\": sandbox killed")}}}},
		{name: "clearing report dir fails", script: commandSandbox{clear: reply{res: contracts.ExecResult{ExitCode: 1}}}},
		{name: "clearing report dir errors", script: commandSandbox{clear: reply{err: errors.New("sandbox is closed")}}},
		{name: "report read: find missing", script: commandSandbox{report: reply{res: contracts.ExecResult{ExitCode: 127}}}},
		{name: "report read errors", script: commandSandbox{report: reply{err: errors.New("sandbox lifetime exceeded")}}},
		{name: "no lint command", cmds: Commands{Build: testCommands.Build, Test: testCommands.Test}},
		{name: "output cannot be stored", script: withReport(passingReport), blobs: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := newFakeSandbox("gating")
			sb.exec = tt.script.exec
			cmds := testCommands
			if tt.cmds.Build != nil {
				cmds = tt.cmds
			}
			var store = newTestBlobs(t)
			if tt.blobs {
				store = failingBlobs{}
			}
			got, err := runCommands(context.Background(), sb, store, testRepoID, cmds)
			if !errors.Is(err, ErrEnvironment) || got != nil {
				t.Fatalf("runCommands = %+v, %v; want nil, ErrEnvironment", got, err)
			}
		})
	}
}

// TestRunCommands_Cancelled: cancellation mid-gate is the context's
// error, not the environment's.
func TestRunCommands_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sb := newFakeSandbox("gating")
	sb.exec = func(req contracts.ExecRequest) (contracts.ExecResult, error) {
		if req.Command[0] == "make" && req.Command[1] == "lint" {
			cancel()
			return contracts.ExecResult{}, fmt.Errorf("exec: %w", errors.Join(context.Canceled, sandbox.ErrOutputTooLarge))
		}
		return contracts.ExecResult{}, nil
	}
	got, err := runCommands(ctx, sb, newTestBlobs(t), testRepoID, testCommands)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrEnvironment) || got != nil {
		t.Fatalf("runCommands = %+v, %v; want nil, context.Canceled", got, err)
	}
}

func TestNotReproducedVerdicts(t *testing.T) {
	want := []verdict{
		{Gate: contracts.GateBuild, Detail: detailNotReproduced},
		{Gate: contracts.GateLint, Detail: detailNotReproduced},
		{Gate: contracts.GateTests, Detail: detailNotReproduced},
	}
	assertVerdicts(t, notReproducedVerdicts(), want)
}
