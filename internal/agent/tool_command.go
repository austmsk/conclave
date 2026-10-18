package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// Audit codes for command tools' own refusals.
const (
	codeCommandArgs    = "invalid_arguments"
	codeConfiguredArgs = "unexpected_arguments"
)

// noSurvivorNote is in every command tool's description: the sandbox kills
// every process after each command, so state lives only in files.
const noSurvivorNote = "No process survives between commands: anything started in the " +
	"background, such as a server, is killed when the command returns."

// runCommand is the run_command tool: it runs a model-supplied command, the
// audited escape hatch (FR-SEC24). The command is an argv, as
// contracts.ExecRequest takes it; a shell is used only if the model asks
// for one, as in ["sh", "-c", "..."].
type runCommand struct {
	env toolEnv
}

type runCommandArgs struct {
	Command        []string `json:"command"`
	TimeoutSeconds float64  `json:"timeout_seconds"`
	WorkDir        string   `json:"workdir"`
}

func newRunCommand(env toolEnv) contracts.Tool { return &runCommand{env: env} }

func (t *runCommand) Name() contracts.ToolName { return contracts.ToolRunCommand }

func (t *runCommand) Schema() json.RawMessage {
	return marshalSchema(map[string]any{
		"type": "object",
		"description": "Runs a command in the workspace and returns its exit code, stdout and stderr. " +
			"Prefer run_tests, run_build and run_lint where they fit; every run_command call " +
			"stands out in the audit. There is no network. " + noSurvivorNote,
		"properties": map[string]any{
			"command": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1,
				"description": `The program and its arguments, one per element, run without a shell. ` +
					`For pipes or redirection, use ["sh", "-c", "..."].`,
			},
			"timeout_seconds": map[string]any{
				"type": "number",
				"description": fmt.Sprintf("Seconds before the command is killed. Omitted, zero, or "+
					"above %[1]d means %[1]d.", int(t.env.maxCommandTimeout.Seconds())),
			},
			"workdir": map[string]any{
				"type":        "string",
				"description": "Directory to run in, relative to the workspace root. Omit for the root.",
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	})
}

// Execute runs the command. A nonzero exit is a normal result the model
// reads; only a refused argument or a sandbox failure is an error.
func (t *runCommand) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	a, err := parseRunCommandArgs(args)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	if a.WorkDir != "" {
		if err := checkPath(a.WorkDir, t.env.denylist); err != nil {
			return contracts.ToolResult{}, fmt.Errorf("run_command workdir: %w", err)
		}
	}
	timeout, err := commandTimeout(ctx, a.TimeoutSeconds, t.env.maxCommandTimeout)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	return execCommand(ctx, sb, contracts.ExecRequest{Command: a.Command, WorkDir: a.WorkDir, Timeout: timeout})
}

func parseRunCommandArgs(raw json.RawMessage) (runCommandArgs, error) {
	var a runCommandArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, refuse(codeCommandArgs, "run_command arguments: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return a, refuse(codeCommandArgs, "run_command arguments must be one JSON object, but more data follows it")
	}
	if len(a.Command) == 0 || a.Command[0] == "" {
		return a, refuse(codeCommandArgs, "run_command needs a command: a non-empty array whose first element is the program")
	}
	for i, arg := range a.Command {
		if strings.ContainsRune(arg, 0) {
			return a, refuse(codeCommandArgs, "command[%d] contains a NUL byte, which no program can receive", i)
		}
	}
	return a, nil
}

// commandTimeout clamps a requested timeout. Zero, negative, or above max
// becomes max, and the result never outlives ctx's deadline, the session's
// wall clock. The clamp matters: a sandbox treats a timeout of zero or less
// as its own, longer limit.
//
// The comparison is done in float seconds before converting, because
// converting an out-of-range float to a Duration is undefined.
func commandTimeout(ctx context.Context, requestedSeconds float64, maxTimeout time.Duration) (time.Duration, error) {
	timeout := maxTimeout
	if requestedSeconds > 0 && requestedSeconds < maxTimeout.Seconds() {
		timeout = time.Duration(requestedSeconds * float64(time.Second))
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return timeout, nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, fmt.Errorf("no wall-clock time left for a command: %w",
			cmp.Or(context.Cause(ctx), context.DeadlineExceeded))
	}
	return min(timeout, remaining), nil
}

// configuredCommand runs a repository's configured command (FR-I4). It
// takes no arguments from the model: any it sends are refused rather than
// ignored, so the attempt is visible in the audit, and the argv run is
// exactly the configured one.
type configuredCommand struct {
	name       contracts.ToolName
	argv       []string
	maxTimeout time.Duration
}

func newConfiguredCommand(name contracts.ToolName, argv []string, env toolEnv) contracts.Tool {
	return &configuredCommand{name: name, argv: append([]string(nil), argv...), maxTimeout: env.maxCommandTimeout}
}

func (t *configuredCommand) Name() contracts.ToolName { return t.name }

func (t *configuredCommand) Schema() json.RawMessage {
	shown, err := json.Marshal(t.argv)
	if err != nil {
		panic(fmt.Sprintf("marshalling %s argv: %v", t.name, err))
	}
	return marshalSchema(map[string]any{
		"type": "object",
		"description": fmt.Sprintf("Runs the repository's %s command, %s, in the workspace root and returns "+
			"its exit code, stdout and stderr. The command comes from the repository's configuration; "+
			"this tool takes no arguments. %s", configuredKind(t.name), shown, noSurvivorNote),
		"properties":           map[string]any{},
		"additionalProperties": false,
	})
}

// Execute runs the configured argv, refusing any arguments the model sent.
func (t *configuredCommand) Execute(ctx context.Context, sb contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	if !noArguments(args) {
		return contracts.ToolResult{}, refuse(codeConfiguredArgs,
			"%s takes no arguments; its command comes from the repository's configuration. Call it with {}", t.name)
	}
	timeout, err := commandTimeout(ctx, 0, t.maxTimeout)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	argv := append([]string(nil), t.argv...)
	return execCommand(ctx, sb, contracts.ExecRequest{Command: argv, Timeout: timeout})
}

// noArguments reports whether raw is empty, null, or an empty object.
func noArguments(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	v, err := decodeJSONValue(raw)
	if err != nil {
		return false
	}
	if v == nil {
		return true
	}
	obj, ok := v.(map[string]any)
	return ok && len(obj) == 0
}

func configuredKind(name contracts.ToolName) string {
	switch name {
	case contracts.ToolRunTests:
		return "test"
	case contracts.ToolRunBuild:
		return "build"
	case contracts.ToolRunLint:
		return "lint"
	default:
		return string(name)
	}
}

// execCommand runs req and formats the result. A sandbox error is returned
// wrapped, so the runner can still classify it by sentinel.
func execCommand(ctx context.Context, sb contracts.Sandbox, req contracts.ExecRequest) (contracts.ToolResult, error) {
	res, err := sb.Exec(ctx, req)
	if err != nil {
		return contracts.ToolResult{}, fmt.Errorf("running %q: %w", req.Command[0], err)
	}
	return contracts.ToolResult{Output: formatExec(res, req.Timeout), ExitCode: res.ExitCode}, nil
}

// formatExec renders a command's result for the model. The exit code comes
// first, then stdout and stderr each under its own header, so the model
// can tell the streams apart and an empty stream is visible as such:
//
//	exit code 1
//	--- stdout ---
//	...
//	--- stderr ---
//	(empty)
//
// A timed-out command says so on the first line. Duration is left out, so
// identical failures produce identical output for the repeat-error limit.
func formatExec(res contracts.ExecResult, timeout time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit code %d", res.ExitCode)
	if res.TimedOut {
		fmt.Fprintf(&b, " (timed out after %v and killed; output may be incomplete)", timeout)
	}
	b.WriteString("\n")
	writeStream(&b, "stdout", res.Stdout)
	writeStream(&b, "stderr", res.Stderr)
	return b.String()
}

func writeStream(b *strings.Builder, name string, data []byte) {
	fmt.Fprintf(b, "--- %s ---\n", name)
	if len(data) == 0 {
		b.WriteString("(empty)\n")
		return
	}
	b.Write(data)
	if data[len(data)-1] != '\n' {
		b.WriteString("\n")
	}
}

// marshalSchema marshals a schema built from static values.
func marshalSchema(s map[string]any) json.RawMessage {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("marshalling tool schema: %v", err))
	}
	return raw
}
