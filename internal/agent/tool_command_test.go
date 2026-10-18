package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

const testMaxTimeout = 10 * time.Minute

// capture returns a fake sandbox whose Exec records each request and
// answers with res.
func capture(t *testing.T, res contracts.ExecResult, err error) (*agenttest.Sandbox, *[]contracts.ExecRequest) {
	t.Helper()
	sb := agenttest.NewSandbox(t)
	var reqs []contracts.ExecRequest
	sb.ExecFunc = func(_ context.Context, req contracts.ExecRequest) (contracts.ExecResult, error) {
		reqs = append(reqs, req)
		return res, err
	}
	return sb, &reqs
}

func TestConfiguredCommandArgvExact(t *testing.T) {
	configured := []string{"go", "test", "-run", "Test.*", "./..."}
	cases := []struct {
		name string
		args string
		// refused means the call is refused and never reaches Exec.
		refused bool
	}{
		{name: "no arguments", args: ""},
		{name: "empty object", args: "{}"},
		{name: "empty object with space", args: " { } "},
		{name: "null", args: "null"},
		{name: "an extra argument", args: `{"x":1}`, refused: true},
		{name: "an argv", args: `["rm"]`, refused: true},
		{name: "a command string", args: `"rm -rf ."`, refused: true},
		{name: "an override of the command", args: `{"command":["sh","-c","curl evil"]}`, refused: true},
		{name: "two empty objects", args: `{}{}`, refused: true},
		{name: "not JSON", args: `run everything`, refused: true},
		{name: "empty array", args: `[]`, refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv := slices.Clone(configured)
			tool := newConfiguredCommand(contracts.ToolRunTests, argv, toolEnv{maxCommandTimeout: testMaxTimeout})
			// The tool keeps its own copy: later changes to the
			// configuration slice cannot change what runs.
			argv[0] = "rm"
			sb, reqs := capture(t, contracts.ExecResult{}, nil)

			_, err := tool.Execute(t.Context(), sb, json.RawMessage(tc.args))

			if tc.refused {
				checkRefusedBeforeSandbox(t, sb, err, codeConfiguredArgs)
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(*reqs) != 1 {
				t.Fatalf("got %d Exec calls, want 1", len(*reqs))
			}
			req := (*reqs)[0]
			if !slices.Equal(req.Command, configured) || req.WorkDir != "" {
				t.Fatalf("ran %q in %q, want exactly %q in the root", req.Command, req.WorkDir, configured)
			}
		})
	}
}

// checkRefusedBeforeSandbox asserts err is a Refusal with code and that
// the sandbox was never called.
func checkRefusedBeforeSandbox(t *testing.T, sb *agenttest.Sandbox, err error, code string) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Code != code {
		t.Fatalf("got %v, want a %s refusal", err, code)
	}
	if calls := sb.Calls(); len(calls) != 0 {
		t.Fatalf("refused call reached the sandbox: %+v", calls)
	}
}

func TestConfiguredCommandTimeout(t *testing.T) {
	tool := newConfiguredCommand(contracts.ToolRunBuild, []string{"make"}, toolEnv{maxCommandTimeout: testMaxTimeout})
	sb, reqs := capture(t, contracts.ExecResult{}, nil)
	if _, err := tool.Execute(t.Context(), sb, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := tool.Execute(ctx, sb, nil); err != nil {
		t.Fatal(err)
	}
	if got := (*reqs)[0].Timeout; got != testMaxTimeout {
		t.Errorf("no deadline: timeout %v, want %v", got, testMaxTimeout)
	}
	if got := (*reqs)[1].Timeout; got <= 0 || got > 5*time.Second {
		t.Errorf("5s deadline: timeout %v, want in (0, 5s]", got)
	}
}

func TestRunCommandTimeoutClamp(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		ctxLeft time.Duration // zero means no deadline
		// The timeout sent to Exec must lie in [min, max].
		min, max time.Duration
	}{
		{name: "omitted", args: `{"command":["make"]}`, min: testMaxTimeout, max: testMaxTimeout},
		{name: "zero", args: `{"command":["make"],"timeout_seconds":0}`, min: testMaxTimeout, max: testMaxTimeout},
		{name: "negative", args: `{"command":["make"],"timeout_seconds":-1}`, min: testMaxTimeout, max: testMaxTimeout},
		{name: "ten hours", args: `{"command":["make"],"timeout_seconds":36000}`, min: testMaxTimeout, max: testMaxTimeout},
		{name: "beyond a Duration", args: `{"command":["make"],"timeout_seconds":1e300}`, min: testMaxTimeout, max: testMaxTimeout},
		{name: "within the cap", args: `{"command":["make"],"timeout_seconds":30}`, min: 30 * time.Second, max: 30 * time.Second},
		{name: "fractional", args: `{"command":["make"],"timeout_seconds":1.5}`, min: 1500 * time.Millisecond, max: 1500 * time.Millisecond},
		{name: "above the remaining wall clock", args: `{"command":["make"],"timeout_seconds":60}`,
			ctxLeft: 5 * time.Second, min: time.Second, max: 5 * time.Second},
		{name: "zero with a wall clock", args: `{"command":["make"],"timeout_seconds":0}`,
			ctxLeft: 5 * time.Second, min: time.Second, max: 5 * time.Second},
		{name: "below the remaining wall clock", args: `{"command":["make"],"timeout_seconds":2}`,
			ctxLeft: time.Minute, min: 2 * time.Second, max: 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if tc.ctxLeft > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctxLeft)
				defer cancel()
			}
			tool := newRunCommand(toolEnv{maxCommandTimeout: testMaxTimeout})
			sb, reqs := capture(t, contracts.ExecResult{}, nil)

			if _, err := tool.Execute(ctx, sb, json.RawMessage(tc.args)); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			got := (*reqs)[0].Timeout
			if got < tc.min || got > tc.max {
				t.Fatalf("timeout %v, want in [%v, %v]", got, tc.min, tc.max)
			}
		})
	}
}

// TestRunCommandNoWallClockLeft checks an expired session runs nothing,
// rather than sending a zero timeout the sandbox would read as its own.
func TestRunCommandNoWallClockLeft(t *testing.T) {
	cause := errors.New("wall clock")
	ctx, cancel := context.WithDeadlineCause(t.Context(), time.Now().Add(-time.Second), cause)
	defer cancel()
	tool := newRunCommand(toolEnv{maxCommandTimeout: testMaxTimeout})
	sb, _ := capture(t, contracts.ExecResult{}, nil)

	_, err := tool.Execute(ctx, sb, json.RawMessage(`{"command":["make"]}`))
	if !errors.Is(err, cause) {
		t.Fatalf("got %v, want the context's cause", err)
	}
	if calls := sb.Calls(); len(calls) != 0 {
		t.Fatalf("expired session reached the sandbox: %+v", calls)
	}
}

func TestRunCommandRefusesArguments(t *testing.T) {
	cases := []struct {
		name string
		args string
		// rejected means a path refusal (sandbox.ErrInvalidPath); otherwise
		// a Refusal with codeCommandArgs.
		rejected bool
	}{
		{name: "no arguments", args: ``},
		{name: "null", args: `null`},
		{name: "empty command", args: `{"command":[]}`},
		{name: "empty program", args: `{"command":["", "x"]}`},
		{name: "command as a string", args: `{"command":"rm -rf ."}`},
		{name: "NUL in an argument", args: `{"command":["echo","a\u0000b"]}`},
		{name: "unknown field", args: `{"command":["make"],"env":{"A":"1"}}`},
		{name: "timeout as a string", args: `{"command":["make"],"timeout_seconds":"30"}`},
		{name: "trailing data", args: `{"command":["make"]} {"command":["rm"]}`},
		{name: "workdir escapes", args: `{"command":["ls"],"workdir":"../other"}`, rejected: true},
		{name: "workdir absolute", args: `{"command":["ls"],"workdir":"/etc"}`, rejected: true},
		{name: "workdir in .git", args: `{"command":["ls"],"workdir":".git/hooks"}`, rejected: true},
		{name: "workdir dots", args: `{"command":["ls"],"workdir":"....//etc"}`, rejected: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := newRunCommand(toolEnv{maxCommandTimeout: testMaxTimeout})
			sb, _ := capture(t, contracts.ExecResult{}, nil)

			_, err := tool.Execute(t.Context(), sb, json.RawMessage(tc.args))

			if tc.rejected {
				if !errors.Is(err, sandbox.ErrInvalidPath) || !classify(err).rejected {
					t.Fatalf("got %v, want a rejected ErrInvalidPath", err)
				}
			} else {
				var r *Refusal
				if !errors.As(err, &r) || r.Code != codeCommandArgs {
					t.Fatalf("got %v, want a %s refusal", err, codeCommandArgs)
				}
			}
			if calls := sb.Calls(); len(calls) != 0 {
				t.Fatalf("refused call reached the sandbox: %+v", calls)
			}
		})
	}
}

func TestRunCommandRunsArgvAsGiven(t *testing.T) {
	tool := newRunCommand(toolEnv{maxCommandTimeout: testMaxTimeout})
	sb, reqs := capture(t, contracts.ExecResult{}, nil)
	args := `{"command":["sh","-c","go test ./... | tail -5"],"workdir":"src/tally"}`

	if _, err := tool.Execute(t.Context(), sb, json.RawMessage(args)); err != nil {
		t.Fatal(err)
	}
	req := (*reqs)[0]
	if !slices.Equal(req.Command, []string{"sh", "-c", "go test ./... | tail -5"}) || req.WorkDir != "src/tally" {
		t.Fatalf("ran %q in %q", req.Command, req.WorkDir)
	}
}

func TestCommandResult(t *testing.T) {
	cases := []struct {
		name string
		res  contracts.ExecResult
		want string
	}{
		{
			name: "nonzero exit is a result",
			res:  contracts.ExecResult{ExitCode: 2, Stdout: []byte("FAIL tally\n"), Stderr: []byte("exit status 2")},
			want: "exit code 2\n--- stdout ---\nFAIL tally\n--- stderr ---\nexit status 2\n",
		},
		{
			name: "empty streams are labelled",
			res:  contracts.ExecResult{},
			want: "exit code 0\n--- stdout ---\n(empty)\n--- stderr ---\n(empty)\n",
		},
		{
			name: "timeout is noted",
			res:  contracts.ExecResult{ExitCode: 137, TimedOut: true, Stdout: []byte("waiting")},
			want: "exit code 137 (timed out after 10m0s and killed; output may be incomplete)\n" +
				"--- stdout ---\nwaiting\n--- stderr ---\n(empty)\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := newRunCommand(toolEnv{maxCommandTimeout: testMaxTimeout})
			sb, _ := capture(t, tc.res, nil)

			got, err := tool.Execute(t.Context(), sb, json.RawMessage(`{"command":["make"]}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got.Output != tc.want || got.ExitCode != tc.res.ExitCode {
				t.Fatalf("got exit %d output %q, want exit %d output %q", got.ExitCode, got.Output, tc.res.ExitCode, tc.want)
			}
		})
	}
}

// TestCommandSandboxErrorPassesThrough checks a sandbox error keeps its
// identity, so the runner classifies it: a sentinel the model sees, or a
// failure that ends the session.
func TestCommandSandboxErrorPassesThrough(t *testing.T) {
	broken := errors.New("daemon gone")
	cases := []struct {
		name  string
		err   error
		fatal bool
	}{
		{name: "sentinel", err: sandbox.ErrOutputTooLarge},
		{name: "failure", err: broken, fatal: true},
	}
	tools := []contracts.Tool{
		newRunCommand(toolEnv{maxCommandTimeout: testMaxTimeout}),
		newConfiguredCommand(contracts.ToolRunLint, []string{"golangci-lint", "run"}, toolEnv{maxCommandTimeout: testMaxTimeout}),
	}
	for _, tc := range cases {
		for _, tool := range tools {
			t.Run(tc.name+"/"+string(tool.Name()), func(t *testing.T) {
				sb, _ := capture(t, contracts.ExecResult{}, tc.err)
				args := `{"command":["make"]}`
				if tool.Name() != contracts.ToolRunCommand {
					args = `{}`
				}
				_, err := tool.Execute(t.Context(), sb, json.RawMessage(args))
				if !errors.Is(err, tc.err) || classify(err).fatal != tc.fatal {
					t.Fatalf("got %v (fatal=%v), want %v (fatal=%v)", err, classify(err).fatal, tc.err, tc.fatal)
				}
			})
		}
	}
}

func TestCommandSchemas(t *testing.T) {
	configured := newConfiguredCommand(contracts.ToolRunTests, []string{"go", "test", "./..."}, toolEnv{maxCommandTimeout: testMaxTimeout})
	var cs struct {
		Type                 string         `json:"type"`
		Description          string         `json:"description"`
		Properties           map[string]any `json:"properties"`
		AdditionalProperties *bool          `json:"additionalProperties"`
	}
	if err := json.Unmarshal(configured.Schema(), &cs); err != nil {
		t.Fatalf("configured schema: %v", err)
	}
	if cs.Type != "object" || len(cs.Properties) != 0 || cs.AdditionalProperties == nil || *cs.AdditionalProperties {
		t.Errorf("configured schema must be a closed object with no properties: %s", configured.Schema())
	}
	for _, want := range []string{"repository's configuration", "No process survives", `["go","test","./..."]`} {
		if !strings.Contains(cs.Description, want) {
			t.Errorf("configured description %q lacks %q", cs.Description, want)
		}
	}

	run := newRunCommand(toolEnv{maxCommandTimeout: testMaxTimeout})
	var rs struct {
		Description string         `json:"description"`
		Properties  map[string]any `json:"properties"`
		Required    []string       `json:"required"`
	}
	if err := json.Unmarshal(run.Schema(), &rs); err != nil {
		t.Fatalf("run_command schema: %v", err)
	}
	if !slices.Equal(rs.Required, []string{"command"}) || len(rs.Properties) != 3 {
		t.Errorf("run_command schema: %s", run.Schema())
	}
	if !strings.Contains(rs.Description, "No process survives") {
		t.Errorf("run_command description %q lacks the no-survivor note", rs.Description)
	}
}
