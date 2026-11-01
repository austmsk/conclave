package activity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/store"
)

// The delivery's pure parts and its read-only queries: the commit
// message, the pull request body, the CI verdict, and what they read from
// the attempt and the plan.

// commitMessage is the task's one squashed commit's message (FR-S17):
// the pr_writer's Conventional Commits subject and body, then the
// platform's trailers. Generated-by is also the commit's AI label
// (FR-C3). It is built once and stored, because it is part of every SHA
// the delivery pushes.
func commitMessage(out agent.PRWriterOutput, taskID, planID string, m commitMeta) string {
	var b strings.Builder
	b.WriteString(out.Subject())
	b.WriteString("\n\n")
	if out.Body != "" {
		b.WriteString(out.Body)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Task: %s\nPlan: %s v%d\nModel: %s\nGenerated-by: conclave\n", taskID, planID, m.PlanVersion, m.Model)
	return b.String()
}

// newSandbox makes a sandbox over the prepared tree at tree. With install
// it goes through CreateWithInstall when the repository configures an
// install phase, which alone has registry access and ends cut off;
// without, the sandbox never has a network, which the finalization
// rebase does not need.
func (a *Activities) newSandbox(ctx context.Context, cfg Repo, tree string, install bool) (contracts.Sandbox, error) {
	sc := cfg.Sandbox
	spec := contracts.SandboxSpec{Image: sc.Image, TreePath: tree, Env: sc.Env, Network: contracts.NetworkNone, Limits: sc.Limits}
	if !install || len(sc.Install) == 0 {
		return a.d.Sandboxes.Create(ctx, spec)
	}
	spec.Network = contracts.NetworkRegistries
	steps := make([]contracts.ExecRequest, 0, len(sc.Install))
	for _, argv := range sc.Install {
		steps = append(steps, contracts.ExecRequest{Command: argv, Timeout: sc.CommandTimeout})
	}
	sb, _, err := a.d.Sandboxes.CreateWithInstall(ctx, spec, steps)
	return sb, err
}

// decodePlanBody decodes the frozen plan body.
func decodePlanBody(pc planContext) (PlanBody, error) {
	var body PlanBody
	if err := json.Unmarshal(pc.Body, &body); err != nil {
		return PlanBody{}, fmt.Errorf("%w: decoding body of plan %s: %v", errNonRetryable, pc.PlanID, err)
	}
	return body, nil
}

// readCommitMeta reads the attempt's finish time and the model that
// answered it, and the plan's version. A passed attempt always has a
// finish time.
func readCommitMeta(ctx context.Context, db store.DBTX, attemptID, planID string) (commitMeta, error) {
	const query = `SELECT a.finished_at, a.model, p.version FROM attempts a, plans p
		WHERE a.id = $1 AND p.id = $2 AND a.finished_at IS NOT NULL`
	var m commitMeta
	err := db.QueryRow(ctx, query, attemptID, planID).Scan(&m.FinishedAt, &m.Model, &m.PlanVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return commitMeta{}, fmt.Errorf("%w: attempt %s has no finish time or plan %s is missing", errNonRetryable, attemptID, planID)
	}
	if err != nil {
		return commitMeta{}, fmt.Errorf("reading commit details of attempt %s: %w", attemptID, err)
	}
	m.FinishedAt = m.FinishedAt.UTC()
	return m, nil
}

// readPlanVersion reads a plan's version.
func readPlanVersion(ctx context.Context, db store.DBTX, planID string) (int, error) {
	var v int
	if err := db.QueryRow(ctx, `SELECT version FROM plans WHERE id = $1`, planID).Scan(&v); err != nil {
		return 0, notFound(err, "plan", planID)
	}
	return v, nil
}

// readAttemptReport reads the attempt's completion report as JSON, or ""
// when it recorded none.
func readAttemptReport(ctx context.Context, db store.DBTX, attemptID string) (string, error) {
	var report string
	err := db.QueryRow(ctx, `SELECT coalesce(report::text, '') FROM attempts WHERE id = $1`, attemptID).Scan(&report)
	if err != nil {
		return "", notFound(err, "attempt", attemptID)
	}
	return report, nil
}

// gateLines summarizes gate results for the pr_writer, one line a gate.
func gateLines(results []contracts.GateResult) []string {
	lines := make([]string, 0, len(results))
	for _, r := range results {
		line := string(r.Gate) + ": " + passWord(r.Passed)
		if r.Flagged {
			line += ", flagged"
		}
		if r.Detail != "" {
			line += " (" + r.Detail + ")"
		}
		lines = append(lines, line)
	}
	return lines
}

// failedGates names the required gates without a passing result.
func failedGates(results []contracts.GateResult) []string {
	passed := map[contracts.GateName]bool{}
	for _, r := range results {
		passed[r.Gate] = r.Passed
	}
	var out []string
	for _, g := range gate.RequiredGates {
		if !passed[g] {
			out = append(out, string(g))
		}
	}
	return out
}

func passWord(ok bool) string {
	if ok {
		return "passed"
	}
	return "failed"
}

// passingConclusions are the check-run conclusions that count as a pass.
var passingConclusions = map[string]bool{"success": true, "neutral": true, "skipped": true}

// evaluateChecks decides CI from the head commit's check runs. Only runs
// by the policy's App count, so another App with checks:write cannot
// satisfy a required check by its name; of several runs of one check the
// newest (highest ID) counts, so a re-run supersedes the run before it.
// CI has completed when every required check has completed, or when one
// has completed without passing, which fails it at once.
func evaluateChecks(p gate.CIPolicy, runs []github.CheckRun) (ChecksResult, []store.CICheck) {
	latest := map[string]github.CheckRun{}
	for _, r := range runs {
		if r.AppSlug != p.CheckApp {
			continue
		}
		if cur, ok := latest[r.Name]; !ok || r.ID > cur.ID {
			latest[r.Name] = r
		}
	}
	checks := make([]store.CICheck, 0, len(p.RequiredChecks))
	var pending, failed []string
	for _, name := range p.RequiredChecks {
		r, ok := latest[name]
		switch {
		case !ok || r.Status != "completed":
			pending = append(pending, name)
			checks = append(checks, store.CICheck{Name: name, ID: r.ID, Conclusion: "pending"})
			continue
		case !passingConclusions[r.Conclusion]:
			failed = append(failed, name+" "+r.Conclusion)
		}
		checks = append(checks, store.CICheck{Name: name, ID: r.ID, Conclusion: r.Conclusion})
	}
	switch {
	case len(failed) > 0:
		return ChecksResult{Completed: true, Detail: "failed " + strings.Join(failed, ", ")}, checks
	case len(pending) > 0:
		return ChecksResult{Detail: "waiting for " + strings.Join(pending, ", ")}, checks
	}
	return ChecksResult{Completed: true, Passed: true}, checks
}

// pullRequestView is what the pull request body shows.
type pullRequestView struct {
	Issue       int
	PlanID      string
	PlanVersion int
	PR          store.PullRequestRecord
	Results     []contracts.GateResult
}

// renderPullRequestBody is the pull request's description (FR-S10). The
// platform writes every section but the pr_writer's summary and risks:
// the AI label first (FR-C3), then the security-relevant changes the
// gates flagged (FR-SEC19, FR-SEC25), the summary and risks, the gate
// evidence for the rebased artifact, the plan, and Closes.
func renderPullRequestBody(v pullRequestView) string {
	var b strings.Builder
	b.WriteString("> [!NOTE]\n> This pull request was generated by Conclave, an AI agent platform. " +
		"The code, this description and the commit were produced by AI and checked by the platform's gates; review it as such.\n\n")
	writeSecuritySection(&b, v.Results)
	fmt.Fprintf(&b, "## Summary\n\n%s\n\n## Risks\n\n%s\n\n", strings.TrimSpace(v.PR.Summary), strings.TrimSpace(v.PR.Risks))
	writeGateEvidence(&b, v)
	fmt.Fprintf(&b, "## Plan\n\nPlan `%s` version %d, approved on #%d at base `%s`. Rebased onto `%s` before this pull request opened.\n\n",
		v.PlanID, v.PlanVersion, v.Issue, v.PR.OriginalBaseSHA, v.PR.BaseSHA)
	fmt.Fprintf(&b, "Closes #%d\n", v.Issue)
	return b.String()
}

// writeSecuritySection lists every flagged gate result, or says none was.
func writeSecuritySection(b *strings.Builder, results []contracts.GateResult) {
	b.WriteString("## Security-relevant changes\n\n")
	n := 0
	for _, r := range results {
		if !r.Flagged {
			continue
		}
		n++
		fmt.Fprintf(b, "- **%s**: %s\n", r.Gate, cell(r.Detail))
	}
	if n == 0 {
		b.WriteString("No gate flagged a security-relevant change.\n")
	}
	b.WriteString("\n")
}

// writeGateEvidence is the table of gate results for the pushed artifact.
func writeGateEvidence(b *strings.Builder, v pullRequestView) {
	mode := ""
	if len(v.Results) > 0 {
		mode = string(v.Results[0].Mode)
	}
	fmt.Fprintf(b, "## Gate evidence\n\nArtifact `%s`, gated in `%s`.\n\n| Gate | Result | Detail |\n|---|---|---|\n",
		v.PR.RebasedArtifactHash, mode)
	for _, r := range v.Results {
		res := passWord(r.Passed)
		if r.Flagged {
			res += ", flagged"
		}
		fmt.Fprintf(b, "| %s | %s | %s |\n", r.Gate, res, cell(r.Detail))
	}
	b.WriteString("\n")
}

// cell makes a platform-written detail safe in a table cell: one line,
// no pipes.
func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", "\\|")
}
