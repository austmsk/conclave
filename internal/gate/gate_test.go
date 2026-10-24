package gate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox/docker"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

// gateAttemptID is the attempt every Gate test gates.
const gateAttemptID = "3f2a9c1e-8b7d-4e6f-a5c4-2b1d0e9f8a7c"

// gatingScript scripts a fakeSandbox through a whole Gate: staging steps
// go to stage, everything else to commands.
type gatingScript struct {
	stage    stagingSandbox
	commands commandSandbox
}

func (s gatingScript) exec(req contracts.ExecRequest) (contracts.ExecResult, error) {
	cmd := strings.Join(req.Command, " ")
	for _, step := range []string{"rm -rf -- {}", "tar -x", "git apply", "git diff"} {
		if strings.Contains(cmd, step) {
			return s.stage.exec(req)
		}
	}
	if req.Command[0] == "mv" {
		return s.stage.exec(req)
	}
	return s.commands.exec(req)
}

// passingScript stages diff so that it reproduces, and passes build,
// lint and tests with a report of two tests.
func passingScript(diff []byte) gatingScript {
	return gatingScript{stage: stagingSandbox{roundTrip: diff}, commands: withReport(passingReport)}
}

// gateHarness is a Gater over fakes, with the artifact frozen in a real
// blob store and a real prepared tree on the worker. inPlace is the
// agent's sandbox; fresh is what the clean_room factory returns.
type gateHarness struct {
	g        *Gater
	results  *fakeResults
	scanner  *fakeScanner
	inPlace  *fakeSandbox
	fresh    *fakeSandbox
	factory  int
	newErr   error
	tree     string
	art      Artifact
	diff     []byte
	policy   Policy
	tier     contracts.SensitivityTier
	commands Commands
}

// newGateHarness freezes diff on a fresh prepared tree. Both sandboxes
// answer with passingScript until a test changes them.
func newGateHarness(t *testing.T, diff []byte) *gateHarness {
	t.Helper()
	tree, base := preparedTree(t)
	blobs := newTestBlobs(t)
	key, err := blobs.Put(context.Background(), testRepoID, Envelope(base, diff))
	if err != nil {
		t.Fatal(err)
	}
	art := testArtifact(base, diff)
	if key != art.Key {
		t.Fatalf("blob key %s, want %s", key, art.Key)
	}
	h := &gateHarness{
		results:  newFakeResults(),
		scanner:  &fakeScanner{},
		inPlace:  newFakeSandbox("agent"),
		fresh:    newFakeSandbox("clean-room"),
		tree:     tree,
		art:      art,
		diff:     diff,
		policy:   testPolicy(t),
		tier:     contracts.TierPublic,
		commands: testCommands,
	}
	h.inPlace.exec = passingScript(diff).exec
	h.fresh.exec = passingScript(diff).exec
	h.g = &Gater{
		Blobs:      blobs,
		Results:    h.results,
		Rejections: fakeRejections{},
		Secrets:    h.scanner,
		Now:        func() time.Time { return time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC) },
	}
	return h
}

// fixtureHarness is newGateHarness over testdata/<name>.diff.
func fixtureHarness(t *testing.T, name string) *gateHarness {
	t.Helper()
	diff, err := os.ReadFile(filepath.Join("testdata", name+".diff"))
	if err != nil {
		t.Fatal(err)
	}
	return newGateHarness(t, diff)
}

func (h *gateHarness) input() Input {
	return Input{
		AttemptID: gateAttemptID,
		Artifact:  h.art,
		TreePath:  h.tree,
		Tier:      h.tier,
		Policy:    h.policy,
		Commands:  h.commands,
		InPlace:   h.inPlace,
		NewSandbox: func(context.Context) (contracts.Sandbox, error) {
			h.factory++
			if h.newErr != nil {
				return nil, h.newErr
			}
			return h.fresh, nil
		},
	}
}

func (h *gateHarness) gate(t *testing.T) (Outcome, error) {
	t.Helper()
	return h.g.Gate(context.Background(), h.input())
}

// gatesWhere lists, in RequiredGates order, the gates whose result
// satisfies keep.
func gatesWhere(rows []contracts.GateResult, keep func(contracts.GateResult) bool) []contracts.GateName {
	var out []contracts.GateName
	for _, name := range RequiredGates {
		for _, r := range rows {
			if r.Gate == name && keep(r) {
				out = append(out, name)
			}
		}
	}
	return out
}

func failedGates(rows []contracts.GateResult) []contracts.GateName {
	return gatesWhere(rows, func(r contracts.GateResult) bool { return !r.Passed })
}

func flaggedGates(rows []contracts.GateResult) []contracts.GateName {
	return gatesWhere(rows, func(r contracts.GateResult) bool { return r.Flagged })
}

// exitOne is passingScript with argv exiting 1.
func exitOne(argv string) *gatingScript {
	s := passingScript(nil)
	s.commands.replies = map[string]reply{argv: {res: contracts.ExecResult{ExitCode: 1}}}
	return &s
}

// gateFixtureCases is one artifact per gate that fails it, or flags it
// for the flag-only gates. A gate after a failure, or skipped because of
// one, fails too.
var gateFixtureCases = []struct {
	name        string
	fixture     string
	findings    []Finding
	rejections  int
	script      *gatingScript // nil: passingScript of the fixture
	wantFailed  []contracts.GateName
	wantFlagged []contracts.GateName
	wantMode    contracts.GatingMode
}{
	{
		name: "diff_size", fixture: "fail_diff_size",
		wantFailed: append([]contracts.GateName{contracts.GateDiffSize, contracts.GateSecrets}, commandGates...),
		wantMode:   contracts.GatingResetInPlace,
	},
	{
		name: "protected_paths", fixture: "fail_protected_paths",
		wantFailed: append([]contracts.GateName{contracts.GateProtected}, commandGates...),
		wantMode:   contracts.GatingResetInPlace,
	},
	{
		name: "gate_weakening", fixture: "fail_gate_weakening",
		wantFailed: append([]contracts.GateName{contracts.GateWeakening}, commandGates...),
		wantMode:   contracts.GatingResetInPlace,
	},
	{
		name: "secrets", fixture: "fail_secrets",
		findings:   []Finding{{RuleID: "conclave-canary", Path: "src/Config.java", Line: 2}},
		wantFailed: append([]contracts.GateName{contracts.GateSecrets}, commandGates...),
		wantMode:   contracts.GatingResetInPlace,
	},
	{
		name: "execution_surface", fixture: "flag_execution_surface",
		wantFlagged: []contracts.GateName{contracts.GateExecSurface},
		wantMode:    contracts.GatingCleanRoom,
	},
	{
		name: "tool_calls", fixture: "pass_static", rejections: 3,
		wantFlagged: []contracts.GateName{contracts.GateToolCalls},
		wantMode:    contracts.GatingCleanRoom,
	},
	{
		name: "build", fixture: "pass_static", script: exitOne("make build"),
		wantFailed: commandGates,
		wantMode:   contracts.GatingResetInPlace,
	},
	{
		name: "lint", fixture: "pass_static", script: exitOne("make lint"),
		wantFailed: commandGates[1:],
		wantMode:   contracts.GatingResetInPlace,
	},
	{
		name: "tests", fixture: "pass_static", script: exitOne("make test"),
		wantFailed: commandGates[2:],
		wantMode:   contracts.GatingResetInPlace,
	},
}

// TestGates_EachGateFailingFixture: every gate has an artifact that fails
// it, or flags it for the flag-only gates, through the whole of Gate. A
// failing gate fails the outcome; a flag passes it, carries the flag and
// moves gating to clean_room. The real gitleaks binary is exercised by
// TestGitleaks_Real.
func TestGates_EachGateFailingFixture(t *testing.T) {
	for _, tc := range gateFixtureCases {
		t.Run(tc.name, func(t *testing.T) {
			h := fixtureHarness(t, tc.fixture)
			h.scanner.findings = tc.findings
			h.g.Rejections = fakeRejections{count: tc.rejections}
			if tc.script != nil {
				s := *tc.script
				s.stage.roundTrip = h.diff
				h.inPlace.exec = s.exec
			}
			out, err := h.gate(t)
			if err != nil {
				t.Fatalf("Gate: %v", err)
			}
			assertOutcome(t, out, tc.wantFailed, tc.wantFlagged, tc.wantMode)
		})
	}
}

// assertOutcome checks exactly which gates failed and flagged, that the
// outcome agrees with them, and the mode every gate ran in.
func assertOutcome(t *testing.T, out Outcome, failed, flagged []contracts.GateName, mode contracts.GatingMode) {
	t.Helper()
	if got := failedGates(out.Results); !slices.Equal(got, failed) {
		t.Errorf("failed gates = %v, want %v", got, failed)
	}
	if got := flaggedGates(out.Results); !slices.Equal(got, flagged) {
		t.Errorf("flagged gates = %v, want %v", got, flagged)
	}
	if out.Passed != (len(failed) == 0) || out.Flagged != (len(flagged) != 0) {
		t.Errorf("outcome passed %t flagged %t, want %t %t", out.Passed, out.Flagged, len(failed) == 0, len(flagged) != 0)
	}
	if out.Mode != mode || len(out.Results) != len(RequiredGates) {
		t.Errorf("mode %s with %d results, want %s with %d", out.Mode, len(out.Results), mode, len(RequiredGates))
	}
}

// TestGate_StaticFailureSkipsSandbox: a failing static gate is recorded
// without staging or running anything, so attacker code never runs for
// an artifact already rejected.
func TestGate_StaticFailureSkipsSandbox(t *testing.T) {
	h := fixtureHarness(t, "fail_protected_paths")
	out, err := h.gate(t)
	if err != nil || out.Passed {
		t.Fatalf("Gate = passed %t, %v; want failed", out.Passed, err)
	}
	if n := len(h.inPlace.commands()) + len(h.fresh.commands()); n != 0 || h.factory != 0 {
		t.Errorf("%d sandbox commands and %d factory calls after a static failure", n, h.factory)
	}
	for _, r := range out.Results {
		if slices.Contains(commandGates, r.Gate) && r.Detail != "not run: a static gate failed" {
			t.Errorf("%s detail = %q", r.Gate, r.Detail)
		}
	}
}

// TestGate_RejectsDeletedTest: an attempt that deletes a test file fails
// gate_weakening, and so the whole artifact.
func TestGate_RejectsDeletedTest(t *testing.T) {
	h := fixtureHarness(t, "fail_gate_weakening")
	out, err := h.gate(t)
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if out.Passed {
		t.Fatal("an artifact deleting a test passed")
	}
	i := slices.IndexFunc(out.Results, func(r contracts.GateResult) bool { return r.Gate == contracts.GateWeakening })
	if i < 0 || out.Results[i].Passed || !strings.Contains(out.Results[i].Detail, "src/FooTest.java") {
		t.Errorf("gate_weakening result = %+v, want failed naming src/FooTest.java", out.Results)
	}
}

// TestGate_EveryResultCarriesCheckedHash: every recorded result names the
// artifact that was checked, and what was checked is the artifact: the
// diff staged into the sandbox is the blob's diff, which hashes, with the
// base, to that name.
func TestGate_EveryResultCarriesCheckedHash(t *testing.T) {
	for _, mode := range []contracts.GatingMode{contracts.GatingResetInPlace, contracts.GatingCleanRoom} {
		t.Run(string(mode), func(t *testing.T) { checkGatedHash(t, mode) })
	}
}

func checkGatedHash(t *testing.T, mode contracts.GatingMode) {
	h := fixtureHarness(t, "pass_static")
	h.policy.Mode = mode
	out, err := h.gate(t)
	if err != nil || !out.Passed || out.Mode != mode {
		t.Fatalf("Gate = %+v, %v; want passed in %s", out, err, mode)
	}
	for _, r := range h.results.all() {
		if r.ArtifactHash != h.art.Hash || r.Mode != mode || r.AttemptID != gateAttemptID {
			t.Errorf("%s row = {hash %s mode %s attempt %s}, want {%s %s %s}", r.Gate, r.ArtifactHash, r.Mode, r.AttemptID, h.art.Hash, mode, gateAttemptID)
		}
	}
	_, stored, err := Load(context.Background(), h.g.Blobs, h.art.Key)
	if err != nil {
		t.Fatal(err)
	}
	staged, untouched := h.inPlace, h.fresh
	if mode == contracts.GatingCleanRoom {
		staged, untouched = h.fresh, h.inPlace
	}
	got := staged.writes[stageDiffName]
	if !bytes.Equal(got, stored) || HashArtifact(h.art.Base, got) != h.art.Hash {
		t.Errorf("staged diff %q is not the artifact's %q", got, stored)
	}
	if n := len(untouched.commands()); n != 0 {
		t.Errorf("the other sandbox ran %d commands", n)
	}
}

// TestGate_CleanRoomClosed: a clean_room sandbox is closed whether gating
// passes, fails or errors; the agent's sandbox is the caller's to close.
func TestGate_CleanRoomClosed(t *testing.T) {
	cases := map[string]func(h *gateHarness){
		"passed": func(*gateHarness) {},
		"failed": func(h *gateHarness) {
			h.fresh.exec = gatingScript{stage: stagingSandbox{roundTrip: []byte("other")}}.exec
		},
		"errored": func(h *gateHarness) { h.fresh.writeErr = errors.New("upload refused") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := fixtureHarness(t, "pass_static")
			h.policy.Mode = contracts.GatingCleanRoom
			setup(h)
			_, _ = h.gate(t)
			if h.fresh.closeCalls != 1 || h.inPlace.closeCalls != 0 {
				t.Errorf("clean_room closed %d times, agent sandbox %d; want 1 and 0", h.fresh.closeCalls, h.inPlace.closeCalls)
			}
		})
	}
}

// TestGate_NotReproducedFails: a diff that does not reproduce on the base
// fails the command gates without running them. It is the attempt's own
// failure, so it is recorded.
func TestGate_NotReproducedFails(t *testing.T) {
	h := fixtureHarness(t, "pass_static")
	h.inPlace.exec = gatingScript{stage: stagingSandbox{applyExit: 1}}.exec
	out, err := h.gate(t)
	if err != nil || out.Passed {
		t.Fatalf("Gate = passed %t, %v; want failed", out.Passed, err)
	}
	for _, r := range out.Results {
		if slices.Contains(commandGates, r.Gate) && r.Detail != detailNotReproduced {
			t.Errorf("%s detail = %q, want %q", r.Gate, r.Detail, detailNotReproduced)
		}
	}
	if h.inPlace.ran("make") >= 0 {
		t.Errorf("commands ran on a diff that did not reproduce: %q", h.inPlace.commands())
	}
}

// TestGate_EnvironmentFailuresAreErrored: when the gating environment
// fails, Gate returns ErrEnvironment and records nothing, so the attempt
// is errored rather than failed and a retry starts clean.
func TestGate_EnvironmentFailuresAreErrored(t *testing.T) {
	cases := map[string]func(t *testing.T, h *gateHarness){
		"sandbox factory": func(_ *testing.T, h *gateHarness) {
			h.tier = contracts.TierSensitive
			h.newErr = errors.New("docker daemon unreachable")
		},
		"upload": func(_ *testing.T, h *gateHarness) { h.inPlace.writeErr = errors.New("upload refused") },
		"build exit 127": func(_ *testing.T, h *gateHarness) {
			s := passingScript(h.diff)
			s.commands.replies = map[string]reply{"make build": {res: contracts.ExecResult{ExitCode: exitNotFound}}}
			h.inPlace.exec = s.exec
		},
		"build killed": func(_ *testing.T, h *gateHarness) {
			s := passingScript(h.diff)
			s.commands.replies = map[string]reply{"make build": {err: docker.ErrKilled}}
			h.inPlace.exec = s.exec
		},
		"tree not prepared": func(t *testing.T, h *gateHarness) {
			h.tree, _ = sandboxtest.NewRepo(t)
		},
		"gitleaks missing": func(t *testing.T, h *gateHarness) {
			g := newFakeGitleaks(t, fakeBehaviour{report: "[]"}).start(t)
			if err := os.Remove(g.exe); err != nil {
				t.Fatal(err)
			}
			h.g.Secrets = g
		},
		"gitleaks swapped": func(t *testing.T, h *gateHarness) {
			g := newFakeGitleaks(t, fakeBehaviour{report: "[]"}).start(t)
			if err := os.Chmod(g.exe, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(g.exe, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			h.g.Secrets = g
		},
		"rejection count": func(_ *testing.T, h *gateHarness) {
			h.g.Rejections = fakeRejections{err: errors.New("connection reset")}
		},
		"reading results": func(_ *testing.T, h *gateHarness) { h.results.forErr = errors.New("connection reset") },
		"recording results": func(_ *testing.T, h *gateHarness) {
			h.results.recordErr = errors.New("connection reset")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := fixtureHarness(t, "pass_static")
			setup(t, h)
			out, err := h.gate(t)
			if !errors.Is(err, ErrEnvironment) {
				t.Fatalf("Gate = %+v, %v; want ErrEnvironment", out, err)
			}
			if rows := h.results.all(); len(rows) != 0 {
				t.Errorf("errored gating recorded %d rows", len(rows))
			}
		})
	}
}

// TestGate_BuildFailureIsFailed: a build that runs and exits non-zero is
// the attempt's failure, not the environment's: the results are recorded
// and the outcome fails.
func TestGate_BuildFailureIsFailed(t *testing.T) {
	h := fixtureHarness(t, "pass_static")
	s := passingScript(h.diff)
	s.commands.replies = map[string]reply{"make build": {res: contracts.ExecResult{ExitCode: 1}}}
	h.inPlace.exec = s.exec
	out, err := h.gate(t)
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if out.Passed || len(h.results.all()) != len(RequiredGates) {
		t.Fatalf("passed %t with %d rows, want failed with %d", out.Passed, len(h.results.all()), len(RequiredGates))
	}
	if got := failedGates(out.Results); !slices.Equal(got, commandGates) {
		t.Errorf("failed gates = %v, want %v", got, commandGates)
	}
}

// TestGateTwiceRunsOnce (RL-2): once a complete result set is recorded, a
// second Gate returns it without touching a sandbox, even if the
// sandbox would now answer differently. The verdict cannot be retried
// into a pass.
func TestGateTwiceRunsOnce(t *testing.T) {
	h := fixtureHarness(t, "pass_static")
	s := passingScript(h.diff)
	s.commands.replies = map[string]reply{"make test": {res: contracts.ExecResult{ExitCode: 1}}}
	h.inPlace.exec = s.exec
	first, err := h.gate(t)
	if err != nil || first.Passed {
		t.Fatalf("first Gate = passed %t, %v; want failed", first.Passed, err)
	}
	execs := len(h.inPlace.commands())
	h.inPlace.exec = passingScript(h.diff).exec

	second, err := h.gate(t)
	if err != nil {
		t.Fatalf("second Gate: %v", err)
	}
	if second.Passed || !slices.Equal(failedGates(second.Results), failedGates(first.Results)) {
		t.Errorf("second Gate = %+v, want the first, failing verdict", second)
	}
	if n := len(h.inPlace.commands()); n != execs || h.results.recordCalls() != 1 || h.scanner.calls() != 1 {
		t.Errorf("second Gate ran: %d new commands, %d Record calls, %d scans", n-execs, h.results.recordCalls(), h.scanner.calls())
	}
}

// TestGate_PartialRecordRunsAgain: rows for only some gates are not a
// verdict. Gate runs, and the rows already held win (first writer wins).
func TestGate_PartialRecordRunsAgain(t *testing.T) {
	h := fixtureHarness(t, "pass_static")
	held := contracts.GateResult{
		AttemptID: gateAttemptID, Gate: contracts.GateLint, Passed: false, ArtifactHash: h.art.Hash,
		Mode: contracts.GatingResetInPlace, Detail: "exit 2", ID: "held",
	}
	h.results.put(held)
	out, err := h.gate(t)
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if out.Passed || !slices.Equal(failedGates(out.Results), []contracts.GateName{contracts.GateLint}) {
		t.Errorf("outcome = %+v, want failed on the held lint row", out)
	}
	if h.results.recordCalls() != 1 || h.inPlace.ran("make build") < 0 {
		t.Errorf("Gate did not run and record: %d Record calls, commands %q", h.results.recordCalls(), h.inPlace.commands())
	}
}

// TestGate_RefusesBadInput: an Input that cannot be gated is a caller
// bug, refused before anything runs, and not an environment failure.
func TestGate_RefusesBadInput(t *testing.T) {
	cases := map[string]func(in *Input){
		"no attempt":         func(in *Input) { in.AttemptID = "" },
		"no artifact":        func(in *Input) { in.Artifact = Artifact{} },
		"no tree":            func(in *Input) { in.TreePath = "" },
		"no factory":         func(in *Input) { in.NewSandbox = nil },
		"no test command":    func(in *Input) { in.Commands.Test = nil },
		"no timeout":         func(in *Input) { in.Commands.Timeout = 0 },
		"unknown tier":       func(in *Input) { in.Tier = "" },
		"wrong hash for key": func(in *Input) { in.Artifact.Hash = strings.Repeat("0", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := fixtureHarness(t, "pass_static")
			in := h.input()
			mutate(&in)
			_, err := h.g.Gate(context.Background(), in)
			if err == nil {
				t.Fatal("Gate accepted a bad Input")
			}
			if name != "wrong hash for key" && errors.Is(err, ErrEnvironment) {
				t.Errorf("caller bug reported as ErrEnvironment: %v", err)
			}
			if n := len(h.inPlace.commands()); n != 0 || len(h.results.all()) != 0 {
				t.Errorf("bad Input ran %d commands and recorded %d rows", n, len(h.results.all()))
			}
		})
	}
}
