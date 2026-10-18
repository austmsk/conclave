package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

// memRecorder is an in-memory Recorder. Like the store, it fails a write
// whose context is done, so a test sees a record the runner made on a
// cancelled context.
type memRecorder struct {
	mu   sync.Mutex
	recs []ToolCallRecord
	// err, when set, fails every write.
	err error
}

func (m *memRecorder) Record(ctx context.Context, rec ToolCallRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.recs = append(m.recs, rec)
	return nil
}

func (m *memRecorder) all() []ToolCallRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.recs)
}

// memBlobs is an in-memory blob.Store with the real store's keys.
type memBlobs struct {
	mu    sync.Mutex
	blobs map[string][]byte
	err   error
}

func (m *memBlobs) Put(ctx context.Context, ns string, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return "", m.err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	key := ns + "/" + hex.EncodeToString(sum[:])
	if m.blobs == nil {
		m.blobs = map[string][]byte{}
	}
	m.blobs[key] = slices.Clone(data)
	return key, nil
}

func (m *memBlobs) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[key]
	if !ok {
		return nil, fmt.Errorf("no blob %s", key)
	}
	return data, nil
}

func (m *memBlobs) all() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [][]byte
	for _, b := range m.blobs {
		out = append(out, b)
	}
	return out
}

// rig is one runner under test with its fakes.
type rig struct {
	sb    *agenttest.Sandbox
	rec   *memRecorder
	blobs *memBlobs
	cfg   Config
}

func newRig(t *testing.T, router contracts.Router) *rig {
	t.Helper()
	g := &rig{sb: agenttest.NewSandbox(t), rec: &memRecorder{}, blobs: &memBlobs{}}
	g.cfg = Config{
		Router:     router,
		Recorder:   g.rec,
		Blobs:      g.blobs,
		AttemptID:  agenttest.AttemptID,
		SessionKey: "wf-1/activity-2/1",
		Commands: Commands{
			Test:  []string{"go", "test", "./..."},
			Build: []string{"go", "build", "./..."},
			Lint:  []string{"golangci-lint", "run"},
		},
		OutputCap:         4096,
		MaxCommandTimeout: time.Minute,
	}
	return g
}

func (g *rig) run(t *testing.T, spec contracts.RoleSpec) (contracts.SessionOutput, error) {
	t.Helper()
	return g.runInput(t, t.Context(), testInput(spec))
}

func (g *rig) runInput(t *testing.T, ctx context.Context, in contracts.SessionInput) (contracts.SessionOutput, error) {
	t.Helper()
	r, err := New(g.cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r.Run(ctx, g.sb, in)
}

// testSpec is the implementer with test-sized limits, edited by edit.
func testSpec(edit func(*contracts.RoleSpec)) contracts.RoleSpec {
	spec := Implementer
	spec.Tools = slices.Clone(Implementer.Tools)
	spec.Limits = contracts.Limits{MaxSteps: 20, MaxTokens: 1_000_000, MaxWallClock: time.Minute, MaxRepeatErrors: 3}
	if edit != nil {
		edit(&spec)
	}
	return spec
}

func testInput(spec contracts.RoleSpec) contracts.SessionInput {
	return contracts.SessionInput{
		RepoID: agenttest.RepoID,
		Spec:   spec,
		Task: contracts.TaskSpec{
			Key:         "T1-count",
			Description: "Count ballots per precinct.",
			Criteria:    []contracts.AcceptanceCriteria{{ID: "AC1", Text: "Each precinct's count is reported."}},
		},
		Profile:   contracts.RepoProfile{FullName: "austmsk/election-tally", Language: "Go"},
		IssueText: "Please count ballots per precinct.",
	}
}

func callOf(id string, name contracts.ToolName, args string) contracts.ToolCall {
	return contracts.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func finishCall(id string) contracts.ToolCall { return callOf(id, contracts.ToolFinish, validReport) }

func readCall(id, path string) contracts.ToolCall {
	return callOf(id, contracts.ToolReadFile, fmt.Sprintf(`{"path":%q}`, path))
}

// turnCalls is a reply making calls. Every turn reports usage, because a
// turn with none is billed at its full reservation.
func turnCalls(calls ...contracts.ToolCall) faketest.Turn {
	return faketest.Turn{Response: faketest.Response{
		ToolCalls: calls, StopReason: contracts.StopToolUse, InputTokens: 100, OutputTokens: 20,
	}}
}

// turnText is a reply with no tool call.
func turnText(stop contracts.StopReason, text string) faketest.Turn {
	return faketest.Turn{Response: faketest.Response{Text: text, StopReason: stop, InputTokens: 100, OutputTokens: 20}}
}

// lastResults is the tool results a request answers its previous turn
// with.
func lastResults(t *testing.T, req llm.ProviderRequest) []contracts.ToolResult {
	t.Helper()
	last := req.Messages[len(req.Messages)-1]
	if last.Role != contracts.MessageUser {
		t.Fatalf("last message is %s, want user", last.Role)
	}
	return last.ToolResults
}

// checkSteps requires audit steps numbered 1..n with the given codes.
func checkSteps(t *testing.T, recs []ToolCallRecord, codes ...string) {
	t.Helper()
	got := make([]string, len(recs))
	for i, r := range recs {
		if r.Step != i+1 {
			t.Errorf("record %d has step %d", i, r.Step)
		}
		if r.AttemptID != agenttest.AttemptID || r.SessionKey == "" || r.OccurredAt.IsZero() {
			t.Errorf("record %d is not keyed or stamped: %+v", i, r)
		}
		got[i] = r.Error
	}
	if !slices.Equal(got, codes) {
		t.Errorf("audit codes %q, want %q", got, codes)
	}
}

func seedFiles(t *testing.T, sb *agenttest.Sandbox, files map[string]string) {
	t.Helper()
	seed(t, sb.Dir, files)
}

func TestRunCompletesTask(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "complete_task.json"))
	if err != nil {
		t.Fatal(err)
	}
	var script faketest.Script
	if err := json.Unmarshal(raw, &script); err != nil {
		t.Fatal(err)
	}
	router, provider := agenttest.Router(t, script.Turns...)
	g := newRig(t, router)
	seedFiles(t, g.sb, map[string]string{"tally/count.go": "package tally\n"})

	out, err := g.run(t, testSpec(nil))
	if err != nil || out.Reason != "" {
		t.Fatalf("Run: reason %q, err %v", out.Reason, err)
	}
	if provider.Remaining() != 0 {
		t.Errorf("%d turns unused", provider.Remaining())
	}
	if out.Report.Summary != "Count ballots per precinct" || !slices.Equal(out.Report.TestsAdded, []string{"TestCountPerPrecinct"}) {
		t.Errorf("report %+v", out.Report)
	}
	written, err := os.ReadFile(filepath.Join(g.sb.Dir, "tally", "count_test.go"))
	if err != nil || !strings.Contains(string(written), "TestCountPerPrecinct") {
		t.Errorf("test file not written: %q, %v", written, err)
	}
	wantUsage := contracts.Usage{InputTokens: 2700, OutputTokens: 240, Steps: 3}
	if out.Usage.CostUSD <= 0 {
		t.Errorf("usage cost %v, want positive", out.Usage.CostUSD)
	}
	out.Usage.CostUSD = 0
	if out.Usage != wantUsage {
		t.Errorf("usage %+v, want %+v", out.Usage, wantUsage)
	}
	wantProv := contracts.Provenance{Role: contracts.RoleImplementer, PromptVersion: "implementer-v1", Model: agenttest.Model, Provider: llm.ProviderAnthropic}
	if out.Provenance != wantProv {
		t.Errorf("provenance %+v, want %+v", out.Provenance, wantProv)
	}
	ids := []string{}
	for _, s := range out.Steps {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []string{"call_1", "call_2", "call_3"}) {
		t.Errorf("steps %v", ids)
	}
	checkSteps(t, g.rec.all(), "", "", "")
}

// TestRequestsCarryRoleAndVersion: every request names the repository,
// role and prompt version, and the static system text.
func TestRequestsCarryRoleAndVersion(t *testing.T) {
	router, _ := agenttest.Router(t, turnCalls(readCall("c1", "a.txt")), turnCalls(finishCall("c2")))
	g := newRig(t, &spyRouter{inner: router, check: func(req contracts.Request) error {
		if req.RepoID != agenttest.RepoID || req.Role != contracts.RoleImplementer ||
			req.PromptVersion != "implementer-v1" || req.System != systemPrompt || len(req.Tools) != 10 {
			return fmt.Errorf("request %+v", req)
		}
		return nil
	}})
	seedFiles(t, g.sb, map[string]string{"a.txt": "a"})
	if out, err := g.run(t, testSpec(nil)); err != nil || out.Reason != "" {
		t.Fatalf("Run: %q, %v", out.Reason, err)
	}
}

// spyRouter checks each request before passing it on.
type spyRouter struct {
	inner contracts.Router
	check func(contracts.Request) error
	// fail, when set, answers a request instead of inner.
	fail func(n int) error
	n    int
}

func (s *spyRouter) Complete(ctx context.Context, req contracts.Request) (contracts.Response, error) {
	s.n++
	if s.check != nil {
		if err := s.check(req); err != nil {
			return contracts.Response{}, err
		}
	}
	if s.fail != nil {
		if err := s.fail(s.n); err != nil {
			return contracts.Response{}, err
		}
	}
	return s.inner.Complete(ctx, req)
}

func (s *spyRouter) Embed(ctx context.Context, repoID string, texts []string) ([][]float32, error) {
	return s.inner.Embed(ctx, repoID, texts)
}

// errNotRecorded fails a request that carries a call not yet audited.
var errNotRecorded = errors.New("request carries a call that was not recorded first")

// recordedFirst fails a request carrying a call or result that rec has
// not recorded.
func recordedFirst(rec *memRecorder) func(contracts.Request) error {
	return func(req contracts.Request) error {
		recorded := map[string]bool{}
		for _, r := range rec.all() {
			recorded[r.CallID] = true
		}
		var ids []string
		for _, m := range req.Messages {
			for _, c := range m.ToolCalls {
				ids = append(ids, c.ID)
			}
			for _, r := range m.ToolResults {
				ids = append(ids, r.CallID)
			}
		}
		for _, id := range ids {
			if !recorded[id] {
				return fmt.Errorf("%w: %s", errNotRecorded, id)
			}
		}
		return nil
	}
}

func TestEveryToolCallRecordedBeforeNextRequest(t *testing.T) {
	router, provider := agenttest.Router(t,
		turnCalls(readCall("c1", "a.txt"), readCall("c2", "missing.txt")),
		turnCalls(callOf("c3", "no_such_tool", `{}`)),
		turnCalls(finishCall("c4")),
	)
	g := newRig(t, nil)
	g.cfg.Router = &spyRouter{inner: router, check: recordedFirst(g.rec)}
	seedFiles(t, g.sb, map[string]string{"a.txt": "a"})
	out, err := g.run(t, testSpec(nil))
	if err != nil || out.Reason != "" {
		t.Fatalf("Run: %q, %v", out.Reason, err)
	}
	if provider.Remaining() != 0 {
		t.Fatalf("%d turns unused", provider.Remaining())
	}
	checkSteps(t, g.rec.all(), "", codeNotFile, codeNotOffered, "")
}

func TestRecordFailureFailsClosed(t *testing.T) {
	router, provider := agenttest.Router(t, turnCalls(readCall("c1", "a.txt")), turnCalls(finishCall("c2")))
	g := newRig(t, router)
	seedFiles(t, g.sb, map[string]string{"a.txt": "a"})
	errDB := errors.New("database is down")
	g.rec.err = errDB

	out, err := g.run(t, testSpec(nil))
	if !errors.Is(err, errDB) {
		t.Fatalf("Run error %v, want the record failure", err)
	}
	if provider.Remaining() != 1 {
		t.Errorf("the model was called again after a failed record (%d turns left)", provider.Remaining())
	}
	if out.Reason != "" {
		t.Errorf("reason %q: a platform failure is not the agent's", out.Reason)
	}
}

func TestBlobPutFailureFailsClosed(t *testing.T) {
	router, provider := agenttest.Router(t, turnCalls(readCall("c1", "a.txt")), turnCalls(finishCall("c2")))
	g := newRig(t, router)
	seedFiles(t, g.sb, map[string]string{"a.txt": "a"})
	errDisk := errors.New("disk full")
	g.blobs.err = errDisk

	out, err := g.run(t, testSpec(nil))
	if !errors.Is(err, errDisk) {
		t.Fatalf("Run error %v, want the blob failure", err)
	}
	if provider.Remaining() != 1 {
		t.Errorf("the model was called again after a failed blob write (%d turns left)", provider.Remaining())
	}
	if n := len(g.rec.all()); n != 0 {
		t.Errorf("%d records written for a call whose output was not stored", n)
	}
	if out.Reason != "" {
		t.Errorf("reason %q: a platform failure is not the agent's", out.Reason)
	}
}

func TestOfferedToolsIntersectAllowlist(t *testing.T) {
	router, provider := agenttest.Router(t, turnCalls(finishCall("c1")))
	g := newRig(t, router)
	g.cfg.Commands = Commands{Test: []string{"go", "test"}}
	spec := testSpec(func(s *contracts.RoleSpec) {
		s.Tools = []contracts.ToolName{contracts.ToolSearch, contracts.ToolReadFile, contracts.ToolRunTests, contracts.ToolFinish}
	})
	if _, err := g.run(t, spec); err != nil {
		t.Fatal(err)
	}
	var names []contracts.ToolName
	for _, tool := range provider.Requests()[0].Tools {
		names = append(names, tool.Name)
		if tool.Description == "" || len(tool.Parameters) == 0 {
			t.Errorf("tool %s sent without a description or parameters", tool.Name)
		}
	}
	want := []contracts.ToolName{contracts.ToolSearch, contracts.ToolReadFile, contracts.ToolRunTests, contracts.ToolFinish}
	if !slices.Equal(names, want) {
		t.Errorf("offered %v, want %v", names, want)
	}
}

// TestUnprovidableToolIsConfigError: a role listing a tool the runner
// cannot provide is refused before any model call.
func TestUnprovidableToolIsConfigError(t *testing.T) {
	cases := []struct {
		name  string
		tools []contracts.ToolName
	}{
		{"command not configured", []contracts.ToolName{contracts.ToolRunLint, contracts.ToolFinish}},
		{"not in the registry", []contracts.ToolName{contracts.ToolRequestDependency, contracts.ToolFinish}},
		{"no finish", []contracts.ToolName{contracts.ToolReadFile}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, provider := agenttest.Router(t, turnCalls(finishCall("c1")))
			g := newRig(t, router)
			g.cfg.Commands = Commands{Test: []string{"go", "test"}}
			_, err := g.run(t, testSpec(func(s *contracts.RoleSpec) { s.Tools = tc.tools }))
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("got %v, want ErrInvalidConfig", err)
			}
			if n := len(provider.Requests()); n != 0 {
				t.Errorf("%d model calls before the configuration was refused", n)
			}
		})
	}
}

func TestConfigRefusesEmptyDenylistEntry(t *testing.T) {
	for _, entry := range []string{"", "/", "//"} {
		g := newRig(t, &spyRouter{})
		g.cfg.Denylist = []string{"secrets", entry}
		if _, err := New(g.cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("denylist entry %q: got %v, want ErrInvalidConfig", entry, err)
		}
	}
}

func TestReadOnlyStripsMutatingTools(t *testing.T) {
	router, provider := agenttest.Router(t, turnCalls(finishCall("c1")))
	g := newRig(t, router)
	if _, err := g.run(t, testSpec(func(s *contracts.RoleSpec) { s.ReadOnly = true })); err != nil {
		t.Fatal(err)
	}
	var names []contracts.ToolName
	for _, tool := range provider.Requests()[0].Tools {
		names = append(names, tool.Name)
	}
	want := []contracts.ToolName{contracts.ToolReadFile, contracts.ToolListDir, contracts.ToolSearch, contracts.ToolFinish}
	if !slices.Equal(names, want) {
		t.Errorf("read-only role offered %v, want %v", names, want)
	}
}

func TestDisallowedCallNeverReachesSandbox(t *testing.T) {
	router, provider := agenttest.Router(t,
		turnCalls(
			callOf("c1", contracts.ToolWriteFile, `{"path":"a.txt","content":"pwned"}`),
			callOf("c2", contracts.ToolRunCommand, `{"command":["sh","-c","id"]}`),
			callOf("c3", "bash", `{"cmd":"id"}`),
		),
		turnCalls(finishCall("c4")),
	)
	g := newRig(t, router)
	g.sb.ExecFunc = func(context.Context, contracts.ExecRequest) (contracts.ExecResult, error) {
		t.Error("a refused call reached Exec")
		return contracts.ExecResult{}, nil
	}
	spec := testSpec(func(s *contracts.RoleSpec) { s.ReadOnly = true })
	out, err := g.run(t, spec)
	if err != nil || out.Reason != "" {
		t.Fatalf("Run: %q, %v", out.Reason, err)
	}
	if calls := g.sb.Calls(); len(calls) != 0 {
		t.Errorf("sandbox calls %+v, want none", calls)
	}
	for _, res := range lastResults(t, provider.Requests()[1]) {
		if !strings.Contains(res.Error, "not available") {
			t.Errorf("result %+v is not a refusal", res)
		}
	}
	recs := g.rec.all()
	checkSteps(t, recs, codeNotOffered, codeNotOffered, codeNotOffered, "")
	for _, r := range recs[:3] {
		if !r.Rejected {
			t.Errorf("refused call %s not marked rejected", r.CallID)
		}
	}
	if out.Usage.Steps != 4 {
		t.Errorf("usage steps %d, want 4: refused calls count", out.Usage.Steps)
	}
}

func TestTwoCallsOneFailing(t *testing.T) {
	router, provider := agenttest.Router(t,
		turnCalls(readCall("c1", "a.txt"), readCall("c2", "missing.txt")),
		turnCalls(finishCall("c3")),
	)
	g := newRig(t, router)
	seedFiles(t, g.sb, map[string]string{"a.txt": "alpha"})
	if out, err := g.run(t, testSpec(nil)); err != nil || out.Reason != "" {
		t.Fatalf("Run: %q, %v", out.Reason, err)
	}
	results := lastResults(t, provider.Requests()[1])
	if len(results) != 2 || results[0].CallID != "c1" || results[1].CallID != "c2" {
		t.Fatalf("results %+v, want c1 then c2", results)
	}
	if results[0].Output != "alpha" || results[0].Error != "" {
		t.Errorf("first result %+v", results[0])
	}
	if results[1].Error == "" || results[1].Output != "" {
		t.Errorf("second result %+v, want an error", results[1])
	}
	checkSteps(t, g.rec.all(), "", codeNotFile, "")
}

func TestRepeatedIdenticalCalls(t *testing.T) {
	router, provider := agenttest.Router(t,
		turnCalls(readCall("c1", "a.txt"), readCall("c2", "a.txt")),
		turnCalls(readCall("c3", "a.txt")),
		turnCalls(finishCall("c4")),
	)
	g := newRig(t, router)
	seedFiles(t, g.sb, map[string]string{"a.txt": "alpha"})
	// Identical successes are not failures: a limit of 2 does not trip.
	out, err := g.run(t, testSpec(func(s *contracts.RoleSpec) { s.Limits.MaxRepeatErrors = 2 }))
	if err != nil || out.Reason != "" {
		t.Fatalf("Run: %q, %v", out.Reason, err)
	}
	results := lastResults(t, provider.Requests()[1])
	if len(results) != 2 || results[0].CallID != "c1" || results[1].CallID != "c2" {
		t.Fatalf("results %+v, want one per ID, in order", results)
	}
	if results[0].Output != "alpha" || results[1].Output != "alpha" {
		t.Errorf("results %+v, want both answered", results)
	}
	recs := g.rec.all()
	checkSteps(t, recs, "", "", "", "")
	if recs[0].CallID != "c1" || recs[1].CallID != "c2" {
		t.Errorf("records %q %q, want c1 c2", recs[0].CallID, recs[1].CallID)
	}
}

func TestOutputTruncatedWithPointer(t *testing.T) {
	router, provider := agenttest.Router(t, turnCalls(readCall("c1", "big.txt")), turnCalls(finishCall("c2")))
	g := newRig(t, router)
	g.cfg.OutputCap = 200
	big := strings.Repeat("0123456789", 100)
	seedFiles(t, g.sb, map[string]string{"big.txt": big})
	if out, err := g.run(t, testSpec(nil)); err != nil || out.Reason != "" {
		t.Fatalf("Run: %q, %v", out.Reason, err)
	}
	res := lastResults(t, provider.Requests()[1])[0]
	if !res.Truncated || len(res.Output) > 200 || !strings.Contains(res.Output, "bytes omitted") {
		t.Errorf("result %d bytes, truncated %v: %q", len(res.Output), res.Truncated, res.Output)
	}
	rec := g.rec.all()[0]
	if !rec.Truncated || rec.ArtifactKey == "" || rec.ArtifactKey != res.ArtifactKey {
		t.Fatalf("record %+v does not point at the full output", rec)
	}
	full, err := g.blobs.Get(t.Context(), rec.ArtifactKey)
	if err != nil || string(full) != big {
		t.Errorf("blob holds %d bytes, %v; want the full output", len(full), err)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		name  string
		s     string
		limit int
	}{
		{"fits", "short", 10},
		{"ascii", strings.Repeat("x", 1000), 120},
		{"multibyte", strings.Repeat("é€😀", 200), 150},
		{"limit below marker", strings.Repeat("é", 100), 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := truncate(tc.s, tc.limit)
			if cut != (len(tc.s) > tc.limit) {
				t.Errorf("truncated %v for %d bytes at limit %d", cut, len(tc.s), tc.limit)
			}
			if len(got) > tc.limit {
				t.Errorf("%d bytes, limit %d", len(got), tc.limit)
			}
			if !strings.HasPrefix(tc.s, got[:min(len(got), 2)]) || !utf8.ValidString(got) {
				t.Errorf("not a valid head of the input: %q", got)
			}
		})
	}
}

// checkNoCanary requires that no record or blob carries canary.
func checkNoCanary(t *testing.T, g *rig, canary string) {
	t.Helper()
	for _, r := range g.rec.all() {
		if strings.Contains(r.Error+string(r.ArgumentsRaw)+r.ArtifactKey, canary) {
			t.Errorf("record %d carries the canary: %+v", r.Step, r)
		}
	}
	for _, b := range g.blobs.all() {
		if strings.Contains(string(b), canary) {
			t.Errorf("a blob carries the canary: %q", b)
		}
	}
}

// TestCanaryNotInAudit: a router or sandbox error carrying a credential
// reaches neither the audit rows nor the blob store.
func TestCanaryNotInAudit(t *testing.T) {
	const canary = "canary_sk_live_0000"
	cases := []struct {
		name string
		// router wraps the replay router.
		router func(inner contracts.Router) contracts.Router
		exec   func(context.Context, contracts.ExecRequest) (contracts.ExecResult, error)
		reason contracts.FailureReason
	}{
		{
			name: "sandbox failure",
			exec: func(context.Context, contracts.ExecRequest) (contracts.ExecResult, error) {
				return contracts.ExecResult{}, errors.New("docker exec: token=" + canary)
			},
			reason: contracts.ReasonSandboxError,
		},
		{
			name: "router failure",
			router: func(inner contracts.Router) contracts.Router {
				return &spyRouter{inner: inner, fail: func(n int) error {
					if n == 2 {
						return fmt.Errorf("upstream said %s: %w", canary, llm.ErrUnavailable)
					}
					return nil
				}}
			},
			exec: func(context.Context, contracts.ExecRequest) (contracts.ExecResult, error) {
				return contracts.ExecResult{ExitCode: 1, Stdout: []byte("FAIL")}, nil
			},
			reason: contracts.ReasonProviderError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var router contracts.Router
			router, _ = agenttest.Router(t, turnCalls(callOf("c1", contracts.ToolRunTests, `{}`)), turnCalls(finishCall("c2")))
			if tc.router != nil {
				router = tc.router(router)
			}
			g := newRig(t, router)
			g.sb.ExecFunc = tc.exec
			out, err := g.run(t, testSpec(nil))
			if err == nil || out.Reason != tc.reason {
				t.Fatalf("Run: %q, %v; want %q and an error", out.Reason, err, tc.reason)
			}
			checkNoCanary(t, g, canary)
		})
	}
}
