package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm/faketest"
	"github.com/austmsk/conclave/internal/store"
)

var _ agent.Recorder = (*store.ToolCalls)(nil)

const auditReport = `{"summary":"Count ballots","files_changed":[],"tests_added":[],"decisions":[],"uncertainties":[]}`

func auditTurn(calls ...contracts.ToolCall) faketest.Turn {
	return faketest.Turn{Response: faketest.Response{
		ToolCalls: calls, StopReason: contracts.StopToolUse, InputTokens: 100, OutputTokens: 20,
	}}
}

func auditCall(id string, name contracts.ToolName, args string) contracts.ToolCall {
	return contracts.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// auditSandbox is a fake sandbox holding a README longer than the output
// cap, a symlinked directory, and a test command that fails.
func auditSandbox(t *testing.T, readme string) *agenttest.Sandbox {
	t.Helper()
	sb := agenttest.NewSandbox(t)
	if err := os.WriteFile(filepath.Join(sb.Dir, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sb.Dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(sb.Dir, "link")); err != nil {
		t.Fatal(err)
	}
	sb.ExecFunc = func(context.Context, contracts.ExecRequest) (contracts.ExecResult, error) {
		// Long enough that the recorded duration cannot round to zero.
		time.Sleep(commandTook)
		return contracts.ExecResult{ExitCode: 1, Stdout: []byte("--- FAIL: TestCount")}, nil
	}
	return sb
}

// commandTook is how long the fake test command runs.
const commandTook = 20 * time.Millisecond

type auditRow struct {
	step      int
	callID    string
	tool      string
	args      string
	errText   string
	rejected  bool
	exitCode  int
	key       string
	truncated bool

	// durationMS is checked apart from the other columns, since only the
	// command's is known.
	durationMS int64
}

// TestToolCallsReachAuditTable runs a session against real Postgres, as
// the application role, and a real blob directory: every kind of call
// lands in tool_calls, in order, and each output pointer resolves.
func TestToolCallsReachAuditTable(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	blobs, err := blob.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	readme := strings.Repeat("Election tally counts ballots. ", 20)
	router, provider := agenttest.Router(t,
		auditTurn(
			auditCall("c1", contracts.ToolReadFile, `{"path":"README.md"}`),
			auditCall("c2", "delete_branch", `{"branch":"main"}`),
			auditCall("c3", contracts.ToolReadFile, `{"path":"../etc/passwd"}`),
			auditCall("c4", contracts.ToolReadFile, `{"path":"link/x.txt"}`),
			auditCall("c5", contracts.ToolRunTests, `{}`),
		),
		auditTurn(auditCall("c6", contracts.ToolFinish, `{"summary":"Count ballots"}`)),
		auditTurn(auditCall("c7", contracts.ToolFinish, auditReport)),
	)
	recorder := store.NewToolCalls(tx)
	runner, err := agent.New(agent.Config{
		Router: router, Recorder: recorder, Blobs: blobs,
		AttemptID: s.attempt, SessionKey: "wf-1/activity-2/1",
		Commands:  agent.Commands{Test: []string{"go", "test", "./..."}, Build: []string{"go", "build"}, Lint: []string{"go", "vet"}},
		OutputCap: 128, MaxCommandTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := contracts.SessionInput{RepoID: agenttest.RepoID, Spec: agent.Implementer, IssueText: "Count ballots."}
	out, err := runner.Run(ctx, auditSandbox(t, readme), in)
	if err != nil || out.Reason != "" || provider.Remaining() != 0 {
		t.Fatalf("Run: reason %q, err %v, %d turns left", out.Reason, err, provider.Remaining())
	}

	rows := readAuditRows(t, ctx, tx, s.attempt)
	// Each row holds the tool and the exact arguments the model sent,
	// including for a tool that does not exist and a rejected path.
	want := []auditRow{
		{step: 1, callID: "c1", tool: "read_file", args: `{"path":"README.md"}`, truncated: true},
		{step: 2, callID: "c2", tool: "delete_branch", args: `{"branch":"main"}`, errText: "tool_not_offered", rejected: true},
		{step: 3, callID: "c3", tool: "read_file", args: `{"path":"../etc/passwd"}`, errText: "invalid_path", rejected: true},
		{step: 4, callID: "c4", tool: "read_file", args: `{"path":"link/x.txt"}`, errText: "invalid_path", rejected: true},
		{step: 5, callID: "c5", tool: "run_tests", args: `{}`, exitCode: 1},
		{step: 6, callID: "c6", tool: "finish", args: `{"summary":"Count ballots"}`, errText: "invalid_report"},
		{step: 7, callID: "c7", tool: "finish", args: auditReport},
	}
	checkAuditRows(t, rows, want)
	if got := rows[4].durationMS; got < commandTook.Milliseconds() {
		t.Errorf("run_tests recorded %d ms, want at least %d", got, commandTook.Milliseconds())
	}
	checkBlob(t, ctx, blobs, rows[0].key, readme)
	checkBlob(t, ctx, blobs, rows[4].key, "--- FAIL: TestCount")
	if n, err := recorder.RejectionCount(ctx, s.attempt); err != nil || n != 3 {
		t.Errorf("rejection count %d, %v; want 3", n, err)
	}
}

func readAuditRows(t *testing.T, ctx context.Context, tx pgx.Tx, attempt string) []auditRow {
	t.Helper()
	rows, err := tx.Query(ctx, `SELECT step, call_id, tool, arguments_raw, coalesce(error, ''), rejected, exit_code,
		coalesce(artifact_key, ''), truncated, duration_ms FROM tool_calls WHERE attempt_id = $1 ORDER BY step`, attempt)
	if err != nil {
		t.Fatalf("reading tool calls: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var args []byte
		if err := rows.Scan(&r.step, &r.callID, &r.tool, &args, &r.errText, &r.rejected, &r.exitCode, &r.key, &r.truncated, &r.durationMS); err != nil {
			t.Fatalf("scanning tool call: %v", err)
		}
		r.args = string(args)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading tool calls: %v", err)
	}
	return out
}

// checkAuditRows compares every column but the artifact key, which must
// be set whenever the call produced output and always when truncated, and
// the duration, which the caller checks.
func checkAuditRows(t *testing.T, got, want []auditRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d audit rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g := got[i]
		if g.truncated && g.key == "" {
			t.Errorf("step %d truncated with no artifact key", g.step)
		}
		g.key, g.durationMS = "", 0
		if g != want[i] {
			t.Errorf("row %d: %+v, want %+v", i, g, want[i])
		}
	}
}

func checkBlob(t *testing.T, ctx context.Context, blobs blob.Store, key, want string) {
	t.Helper()
	if key == "" {
		t.Fatal("no artifact key")
	}
	data, err := blobs.Get(ctx, key)
	if err != nil {
		t.Fatalf("resolving %s: %v", key, err)
	}
	if !strings.Contains(string(data), want) {
		t.Errorf("blob %s holds %q, want it to contain %q", key, data, want)
	}
}
