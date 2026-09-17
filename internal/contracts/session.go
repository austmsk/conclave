package contracts

import "context"

// SessionInput is everything an agent receives. Context is supplied by the
// platform, never fetched by the agent: an agent cannot decide to read
// something its tool allowlist does not cover.
//
// What is deliberately absent matters as much as what is present. An
// implementer never receives hidden tests, another attempt's work or
// transcript, the platform's own repository, or history before the base
// commit. Encoding those exclusions now means competition can be added in
// Milestone 3 without changing this contract.
type SessionInput struct {
	RepoID  string
	Spec    RoleSpec
	Profile RepoProfile
	Task    TaskSpec
	Limits  Constraints

	// IssueText is untrusted and arrives already quarantined.
	IssueText string

	BaseCommit string

	// PriorFailure is set only on a retry, carrying the previous attempt's
	// gate failures so the next try has something to work from.
	PriorFailure string
}

// SessionOutput is the result of running one agent session. The diff is not
// here: it is extracted from the sandbox by platform code, because what the
// agent says it did and what it actually did are separate facts.
type SessionOutput struct {
	Report     CompletionReport
	Provenance Provenance
	Usage      Usage

	// Steps is the sequence of tool calls, recorded by the worker for the
	// audit trail (FR-SEC24).
	Steps []ToolCall

	// Reason is set when the session ended for a reason other than finish.
	Reason FailureReason
}

// AgentRunner executes one agent session: send context and tools to the model,
// execute requested tools, feed results back, repeat until finish or a limit.
//
// Implementations enforce Limits themselves rather than asking the model to
// respect them, validate output against the role's schema with exactly one
// repair attempt, and stop after MaxRepeatErrors identical failures.
type AgentRunner interface {
	Run(ctx context.Context, sb Sandbox, in SessionInput) (SessionOutput, error)
}
