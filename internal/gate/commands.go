package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

// Commands are one repository's build, lint and test commands, as argv
// lists run in the workspace root (FR-I4). Test must write a JUnit XML
// report under JUnitDir.
type Commands struct {
	Build []string
	Lint  []string
	Test  []string

	// Timeout bounds each command. Zero means the sandbox's own limit.
	Timeout time.Duration
}

// JUnitDir is where the test command writes its JUnit XML report. It is
// under /tmp, so nothing lands in the diff, and it is deleted before the
// tests run.
const JUnitDir = "/tmp/conclave-junit"

// commandGates are the command gates, in the order they run.
var commandGates = []contracts.GateName{contracts.GateBuild, contracts.GateLint, contracts.GateTests}

// Details for command gates that never ran. They are verdicts of their
// own, so a caller always has one per command gate.
const (
	detailEarlierFailed = "not run: earlier gate failed"
	detailNotReproduced = "not run: artifact does not reproduce on base"
)

// runCommands runs build, lint and tests in the staged workspace, in that
// order, and stops at the first failure. It always returns three
// verdicts, one per command gate in commandGates order: a gate after a
// failure gets Passed=false and detailEarlierFailed.
//
// Each command's full stdout and stderr go to the blob store under
// repoID, keyed in the verdict; Detail is written by the platform alone.
// The tests gate passes only when the command exits 0 and the JUnit
// report it wrote under JUnitDir parses, shows at least one test ran,
// and shows none failed or errored (decision 9).
//
// An error means no verdict could be reached (classifyExec): it wraps
// ErrEnvironment, or the context's error, and no verdicts are returned.
func runCommands(ctx context.Context, sb contracts.Sandbox, blobs blob.Store, repoID string, cmds Commands) ([]verdict, error) {
	argvs := [][]string{cmds.Build, cmds.Lint, cmds.Test}
	out := make([]verdict, 0, len(commandGates))
	failed := false
	for i, gate := range commandGates {
		if failed {
			out = append(out, verdict{Gate: gate, Detail: detailEarlierFailed})
			continue
		}
		v, err := runGate(ctx, sb, blobs, repoID, gate, argvs[i], cmds.Timeout)
		if err != nil {
			return nil, err
		}
		failed = !v.Passed
		out = append(out, v)
	}
	return out, nil
}

// notReproducedVerdicts are the command verdicts for an artifact whose
// recorded diff did not reproduce on its base: none of the commands ran.
func notReproducedVerdicts() []verdict {
	out := make([]verdict, 0, len(commandGates))
	for _, gate := range commandGates {
		out = append(out, verdict{Gate: gate, Detail: detailNotReproduced})
	}
	return out
}

// runGate runs one command gate and stores its output.
func runGate(ctx context.Context, sb contracts.Sandbox, blobs blob.Store, repoID string, gate contracts.GateName, argv []string, timeout time.Duration) (verdict, error) {
	if len(argv) == 0 {
		return verdict{}, environmentf("no %s command configured", gate)
	}
	start := time.Now()
	isTests := gate == contracts.GateTests
	if isTests {
		if err := clearReportDir(ctx, sb); err != nil {
			return verdict{}, err
		}
	}
	res, execErr := sb.Exec(ctx, contracts.ExecRequest{Command: argv, Timeout: timeout})
	detail, err := classifyExec(ctx, string(gate)+" command", res, execErr)
	if err != nil {
		return verdict{}, err
	}
	passed := detail == ""
	if passed {
		detail = "exit 0"
	}
	var report []byte
	if isTests && passed {
		var rep reportOutcome
		if rep, err = readReport(ctx, sb); err != nil {
			return verdict{}, err
		}
		report, detail, passed = rep.raw, rep.detail, rep.passed
	}
	key, err := storeOutput(ctx, blobs, repoID, argv, res, report)
	if err != nil {
		return verdict{}, err
	}
	return verdict{Gate: gate, Passed: passed, Detail: detail, Key: key, Duration: time.Since(start)}, nil
}

// clearReportDir deletes JUnitDir, so the only report the tests gate can
// read is one this run of the test command wrote.
func clearReportDir(ctx context.Context, sb contracts.Sandbox) error {
	res, err := sb.Exec(ctx, contracts.ExecRequest{Command: []string{"rm", "-rf", "--", JUnitDir}})
	if err != nil {
		return stepError(ctx, "clearing "+JUnitDir, err)
	}
	if res.ExitCode != 0 {
		return environmentf("clearing %s: exit %d", JUnitDir, res.ExitCode)
	}
	return nil
}

// reportOutcome is the tests gate's verdict on the JUnit report.
type reportOutcome struct {
	raw    []byte
	passed bool
	detail string
}

// readReport reads every regular *.xml file under JUnitDir, concatenated,
// and judges it. find skips symlinks and anything not a regular file, so
// a planted link or FIFO is ignored rather than followed. A missing
// directory, an unreadable file or output past the backend's limit fails
// the gate; only a command that cannot run at all is an environment
// failure.
func readReport(ctx context.Context, sb contracts.Sandbox) (reportOutcome, error) {
	res, err := sb.Exec(ctx, contracts.ExecRequest{
		Command: []string{"find", JUnitDir, "-type", "f", "-name", "*.xml", "-exec", "cat", "--", "{}", "+"},
	})
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, sandbox.ErrOutputTooLarge) {
			return reportOutcome{detail: "JUnit report exceeded output limit"}, nil
		}
		return reportOutcome{}, stepError(ctx, "reading JUnit report", err)
	}
	switch {
	case res.ExitCode == exitNotExecutable || res.ExitCode == exitNotFound:
		return reportOutcome{}, environmentf("reading JUnit report: exit %d: find or cat missing from the image", res.ExitCode)
	case res.ExitCode != 0:
		return reportOutcome{detail: "exit 0 but no JUnit report"}, nil
	}
	return judgeReport(res.Stdout), nil
}

// judgeReport turns a JUnit report into the tests gate's verdict.
func judgeReport(raw []byte) reportOutcome {
	out := reportOutcome{raw: raw}
	c, err := parseJUnit(raw)
	switch {
	case errors.Is(err, errNoReport):
		out.detail = "exit 0 but no JUnit report"
	case err != nil:
		out.detail = "JUnit report unreadable"
	case c.Ran() == 0:
		out.detail = fmt.Sprintf("JUnit report shows no tests ran (%d skipped)", c.Skipped)
	case c.Failed > 0:
		out.detail = fmt.Sprintf("%d of %d tests failed or errored", c.Failed, c.Ran())
	case c.DeclaredFailed > 0:
		out.detail = fmt.Sprintf("JUnit report declares %d failures or errors", c.DeclaredFailed)
	default:
		out.passed = true
		out.detail = fmt.Sprintf("%d tests ran, 0 failed, %d skipped", c.Ran(), c.Skipped)
	}
	return out
}

// storeOutput writes a command's full output, and the JUnit report when
// there is one, to the blob store and returns the key.
func storeOutput(ctx context.Context, blobs blob.Store, repoID string, argv []string, res contracts.ExecResult, report []byte) (string, error) {
	key, err := blobs.Put(ctx, repoID, formatOutput(argv, res, report))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("storing command output: %w", ctxErr)
		}
		return "", fmt.Errorf("%w: storing command output: %w", ErrEnvironment, err)
	}
	return key, nil
}

// formatOutput lays out one command's evidence. The header lines are the
// platform's; everything after a section marker is the sandbox's, byte
// for byte.
func formatOutput(argv []string, res contracts.ExecResult, report []byte) []byte {
	var b bytes.Buffer
	b.WriteString("command:")
	for _, a := range argv {
		b.WriteString(" " + strconv.Quote(a))
	}
	fmt.Fprintf(&b, "\nexit: %d\ntimed out: %t\nduration: %s\n", res.ExitCode, res.TimedOut, res.Duration)
	b.WriteString("== stdout ==\n")
	b.Write(res.Stdout)
	b.WriteString("\n== stderr ==\n")
	b.Write(res.Stderr)
	if report != nil {
		b.WriteString("\n== junit report ==\n")
		b.Write(report)
	}
	return b.Bytes()
}
