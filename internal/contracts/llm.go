package contracts

import (
	"context"
	"encoding/json"
)

// SensitivityTier governs where a repository's code may go. It is enforced at
// the router, which every model request passes through, so there is exactly
// one place to get this right (FR-SEC12, FR-SEC13).
type SensitivityTier string

const (
	TierPublic    SensitivityTier = "public"
	TierPrivate   SensitivityTier = "private"
	TierSensitive SensitivityTier = "sensitive"
	TierLocalOnly SensitivityTier = "local_only"
)

// MessageRole is the conversational role of a message.
type MessageRole string

const (
	MessageUser      MessageRole = "user"
	MessageAssistant MessageRole = "assistant"
)

// Message is one turn. Untrusted content — issue text, repository files,
// comments — is wrapped in nonce-delimited blocks with provenance labels
// before it reaches this type, and never appears in System instructions
// (FR-SEC6, FR-SEC28).
type Message struct {
	Role MessageRole `json:"role"`

	Text string `json:"text,omitempty"`

	ToolCalls   []ToolCall   `json:"tool_calls,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
}

// ToolSchema is a tool definition sent to a provider. Providers express tool
// calling differently; adapters normalize to this shape, which is why adapter
// bugs surface as strange agent behaviour rather than obvious errors (RL-6).
type ToolSchema struct {
	Name        ToolName        `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Request is a model call. It always carries RepoID, because the tier check
// cannot be performed without knowing whose code is being sent.
type Request struct {
	RepoID string
	Role   Role

	System   string
	Messages []Message
	Tools    []ToolSchema

	MaxTokens int
}

// StopReason explains why generation ended.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
)

// Response is a normalized model reply.
type Response struct {
	Provenance Provenance
	Usage      Usage

	Text      string
	ToolCalls []ToolCall

	StopReason StopReason
}

// Router selects a model for a role, enforces the repository's sensitivity
// tier, applies budgets and rate limits, records cost, and normalizes provider
// differences.
//
// Implementations must fail closed: a repository with no tier, or a selected
// model whose provider the tier disallows, is an error and never a fallback to
// something permissive.
type Router interface {
	Complete(ctx context.Context, req Request) (Response, error)

	// Embed produces vectors for code search. Subject to the same tier check;
	// an embedding call that bypasses the router defeats the whole scheme.
	Embed(ctx context.Context, repoID string, texts []string) ([][]float32, error)
}
