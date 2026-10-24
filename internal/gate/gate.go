package gate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
)

// Results persists gate results. store.GateResults implements it using
// only contracts types.
type Results interface {
	// Record inserts every result in one transaction, keeping any row
	// already stored under the same (attempt, gate, artifact hash): the
	// first writer wins (RL-2). It returns the rows as stored.
	Record(ctx context.Context, results []contracts.GateResult) ([]contracts.GateResult, error)

	// ForArtifact returns every result recorded for one attempt and
	// artifact hash, ordered by gate name.
	ForArtifact(ctx context.Context, attemptID, artifactHash string) ([]contracts.GateResult, error)
}

// RejectionCounter counts an attempt's tool calls refused on security
// grounds (FR-SEC29). store.ToolCalls implements it.
type RejectionCounter interface {
	RejectionCount(ctx context.Context, attemptID string) (int, error)
}

// Input is one attempt's artifact and everything needed to gate it.
type Input struct {
	AttemptID string
	Artifact  Artifact

	// TreePath is the prepared tree on the worker the attempt started
	// from. Its HEAD must be Artifact.Base. Staging streams it into the
	// gating sandbox; the caller keeps it until Gate returns.
	TreePath string

	Tier     contracts.SensitivityTier
	Policy   Policy
	Commands Commands

	// InPlace is the agent's sandbox, reset and reused for
	// reset_in_place. Nil forces clean_room. A sandbox handed to Gate is
	// never handed back to an agent.
	InPlace contracts.Sandbox

	// NewSandbox creates a fresh sandbox for clean_room. Gate closes it.
	NewSandbox func(ctx context.Context) (contracts.Sandbox, error)

	ForceCleanRoom bool
}

// Outcome is the verdict on one artifact.
type Outcome struct {
	Passed  bool
	Flagged bool
	Mode    contracts.GatingMode
	Results []contracts.GateResult
}

// Gater runs the gates and records their results.
type Gater struct {
	Blobs      blob.Store
	Results    Results
	Rejections RejectionCounter
	Secrets    SecretScanner

	// Now stamps results; nil means time.Now.
	Now func() time.Time
}

// verdict is one gate's outcome before it becomes a GateResult.
type verdict struct {
	Gate    contracts.GateName
	Passed  bool
	Flagged bool

	// Detail is a short summary written by the platform. Sandbox output
	// never goes here; it goes to the blob store under Key.
	Detail   string
	Key      string
	Duration time.Duration
}

// RequiredGates are the gates every artifact must have a passing result
// for before it may be pushed. Flag-only gates are included: they pass
// and carry the flag.
var RequiredGates = []contracts.GateName{
	contracts.GateDiffSize,
	contracts.GateProtected,
	contracts.GateWeakening,
	contracts.GateExecSurface,
	contracts.GateToolCalls,
	contracts.GateSecrets,
	contracts.GateBuild,
	contracts.GateLint,
	contracts.GateTests,
}

// Gate runs every gate against in.Artifact, or returns the results
// already recorded for it.
//
// The order is fixed: load the artifact through blob.Get, which re-hashes
// it; run every static gate; if any failed, record and return without
// touching a sandbox; otherwise choose the mode, stage base plus diff, and
// run build, lint and tests. Every result is recorded in one transaction.
//
// An error wrapping ErrEnvironment means the gating environment failed,
// not the attempt: nothing is recorded, and a retry starts clean
// (FR-SEC27). Any other error is a bad Input or a cancelled context.
func (g *Gater) Gate(ctx context.Context, in Input) (Outcome, error) {
	if err := checkInput(in); err != nil {
		return Outcome{}, fmt.Errorf("gating attempt %s: %w", in.AttemptID, err)
	}
	prior, done, err := g.recorded(ctx, in.AttemptID, in.Artifact.Hash)
	if err != nil || done {
		return prior, err
	}
	art, diff, err := Load(ctx, g.Blobs, in.Artifact.Key)
	if err != nil {
		return Outcome{}, fmt.Errorf("gating attempt %s: %w", in.AttemptID, err)
	}
	if art.Hash != in.Artifact.Hash || art.Base != in.Artifact.Base {
		return Outcome{}, environmentf("gating attempt %s: stored artifact %s does not match the one requested", in.AttemptID, art.Key)
	}
	static, err := g.runStatic(ctx, in, diff)
	if err != nil {
		return Outcome{}, fmt.Errorf("gating attempt %s: %w", in.AttemptID, err)
	}
	mode, err := SelectMode(in.Tier, in.Policy, in.InPlace != nil, in.ForceCleanRoom, anyFlagged(static))
	if err != nil {
		return Outcome{}, fmt.Errorf("gating attempt %s: %w", in.AttemptID, err)
	}
	commands := notRun("not run: a static gate failed")
	if allPassed(static) {
		if commands, err = g.runInSandbox(ctx, in, art, diff, mode); err != nil {
			return Outcome{}, fmt.Errorf("gating attempt %s in %s: %w", in.AttemptID, mode, err)
		}
	}
	return g.record(ctx, in.AttemptID, art, mode, append(static, commands...))
}

// checkInput refuses an Input that cannot be gated at all. These are
// caller bugs, not environment failures.
func checkInput(in Input) error {
	switch {
	case in.AttemptID == "":
		return errors.New("no attempt ID")
	case in.Artifact.Key == "" || in.Artifact.Hash == "":
		return errors.New("no frozen artifact")
	case in.TreePath == "":
		return errors.New("no prepared tree")
	case in.NewSandbox == nil:
		return errors.New("no sandbox factory: clean_room needs one")
	case len(in.Commands.Build) == 0 || len(in.Commands.Lint) == 0 || len(in.Commands.Test) == 0:
		return errors.New("build, lint and test commands are all required")
	case in.Commands.Timeout <= 0:
		return errors.New("no command timeout")
	}
	return nil
}

// recorded returns the outcome already stored for this attempt and
// artifact when every required gate has a row, so a retried Gate returns
// the first run's verdict instead of running again (RL-2).
func (g *Gater) recorded(ctx context.Context, attemptID, hash string) (Outcome, bool, error) {
	rows, err := g.Results.ForArtifact(ctx, attemptID, hash)
	if err != nil {
		return Outcome{}, false, environmentf("reading recorded gate results for attempt %s: %v", attemptID, err)
	}
	if !complete(rows) {
		return Outcome{}, false, nil
	}
	out, err := outcomeOf(rows, hash)
	if err != nil {
		return Outcome{}, false, fmt.Errorf("reading recorded gate results for attempt %s: %w", attemptID, err)
	}
	return out, true, nil
}

// runStatic runs the gates that need only the diff: all of them, so the
// owner sees every problem at once. A diff the parser refuses fails
// closed, as the attempt's own failure. Secrets are not scanned when the
// diff is over its size limit, so a huge diff cannot knock over the
// scanner.
func (g *Gater) runStatic(ctx context.Context, in Input, diff []byte) ([]verdict, error) {
	changes, err := ParseDiff(diff)
	if err != nil {
		return unparsable(err), nil
	}
	rejections, err := g.Rejections.RejectionCount(ctx, in.AttemptID)
	if err != nil {
		return nil, environmentf("counting rejected tool calls: %v", err)
	}
	size := checkDiffSize(changes, len(diff), in.Policy.Limits)
	vs := []verdict{
		size,
		checkProtected(changes, in.Policy),
		checkWeakening(changes, in.Policy),
		checkExecSurface(changes, in.Policy),
		checkToolCalls(rejections, in.Policy),
	}
	if !size.Passed {
		return append(vs, verdict{Gate: contracts.GateSecrets, Detail: "not run: diff exceeds its size limit"}), nil
	}
	secrets, err := checkSecrets(ctx, g.Secrets, changes)
	if err != nil {
		return nil, err
	}
	return append(vs, secrets), nil
}

// unparsable fails every static gate for a diff ParseDiff refused. The
// parser's error names the problem, never diff content.
func unparsable(err error) []verdict {
	detail := "diff could not be parsed: " + err.Error()
	gates := []contracts.GateName{
		contracts.GateDiffSize, contracts.GateProtected, contracts.GateWeakening,
		contracts.GateExecSurface, contracts.GateToolCalls, contracts.GateSecrets,
	}
	vs := make([]verdict, len(gates))
	for i, name := range gates {
		vs[i] = verdict{Gate: name, Detail: detail}
	}
	return vs
}

// runInSandbox stages base plus the recorded diff and runs the command
// gates. A clean_room sandbox is closed on every path, from a context
// that survives cancellation (RL-3). The agent's own sandbox is the
// caller's to close.
func (g *Gater) runInSandbox(ctx context.Context, in Input, art Artifact, diff []byte, mode contracts.GatingMode) ([]verdict, error) {
	sb := in.InPlace
	if mode == contracts.GatingCleanRoom {
		fresh, err := in.NewSandbox(ctx)
		if err != nil {
			return nil, environmentf("creating clean_room sandbox: %v", err)
		}
		defer func() { _ = fresh.Close(context.WithoutCancel(ctx)) }()
		sb = fresh
	}
	reproduced, err := stage(ctx, sb, in.TreePath, art, diff)
	if err != nil {
		return nil, err
	}
	if !reproduced {
		return notReproducedVerdicts(), nil
	}
	return runCommands(ctx, sb, g.Blobs, art.RepoID, in.Commands)
}

// notRun is the three command verdicts for gates that never ran. They
// fail: a gate that did not run proves nothing.
func notRun(detail string) []verdict {
	return []verdict{
		{Gate: contracts.GateBuild, Detail: detail},
		{Gate: contracts.GateLint, Detail: detail},
		{Gate: contracts.GateTests, Detail: detail},
	}
}

// record stores every verdict in one transaction and returns the outcome
// as stored. When a concurrent or earlier run got there first, its rows
// win and are what this returns.
func (g *Gater) record(ctx context.Context, attemptID string, art Artifact, mode contracts.GatingMode, vs []verdict) (Outcome, error) {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	at := now()
	results := make([]contracts.GateResult, len(vs))
	for i, v := range vs {
		results[i] = newResult(attemptID, art, mode, v, at)
	}
	stored, err := g.Results.Record(ctx, results)
	if err != nil {
		return Outcome{}, environmentf("recording gate results for attempt %s: %v", attemptID, err)
	}
	out, err := outcomeOf(stored, art.Hash)
	if err != nil {
		return Outcome{}, fmt.Errorf("recording gate results for attempt %s: %w", attemptID, err)
	}
	return out, nil
}

// complete reports whether rows hold a result for every required gate.
func complete(rows []contracts.GateResult) bool {
	have := make(map[contracts.GateName]bool, len(rows))
	for _, r := range rows {
		have[r.Gate] = true
	}
	for _, name := range RequiredGates {
		if !have[name] {
			return false
		}
	}
	return true
}

// outcomeOf summarizes stored rows. It passes only when every required
// gate has a passing row for this hash; any row for another hash is a
// store bug and an error.
func outcomeOf(rows []contracts.GateResult, hash string) (Outcome, error) {
	out := Outcome{Results: rows, Passed: complete(rows)}
	for _, r := range rows {
		if r.ArtifactHash != hash {
			return Outcome{}, fmt.Errorf("gate result %s carries hash %s, want %s", r.Gate, r.ArtifactHash, hash)
		}
		out.Mode = r.Mode
		out.Passed = out.Passed && r.Passed
		out.Flagged = out.Flagged || r.Flagged
	}
	return out, nil
}

func allPassed(vs []verdict) bool {
	for _, v := range vs {
		if !v.Passed {
			return false
		}
	}
	return true
}

func anyFlagged(vs []verdict) bool {
	for _, v := range vs {
		if v.Flagged {
			return true
		}
	}
	return false
}

// newResult is the only constructor of a GateResult, so every result
// carries the hash of the artifact it checked.
func newResult(attemptID string, art Artifact, mode contracts.GatingMode, v verdict, now time.Time) contracts.GateResult {
	return contracts.GateResult{
		AttemptID:    attemptID,
		Gate:         v.Gate,
		Passed:       v.Passed,
		Flagged:      v.Flagged,
		ArtifactHash: art.Hash,
		Mode:         mode,
		Detail:       v.Detail,
		ArtifactKey:  v.Key,
		RanAt:        now,
		Duration:     v.Duration,
	}
}
