package llm

import (
	"context"
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

// Provider is one model vendor's API, normalized. Adapters translate
// ProviderRequest into the vendor's wire format and the reply back into
// ProviderResponse; nothing outside an adapter knows the wire format (FR-I2).
//
// A ProviderRequest carries no repository identity. The tier check happens in
// the router before an adapter is called, so an adapter cannot be the place
// where it is forgotten.
type Provider interface {
	Complete(ctx context.Context, req ProviderRequest) (ProviderResponse, error)
}

// Embedder is implemented by providers that serve embedding models.
type Embedder interface {
	Embed(ctx context.Context, model string, texts []string) (EmbedResponse, error)
}

// EmbedResponse carries the vectors and the usage needed to charge for them.
type EmbedResponse struct {
	ID          string
	Vectors     [][]float32
	InputTokens int
}

// ProviderRequest is a model call after routing: the model is chosen and
// MaxTokens is already clamped to the caller's remaining budget.
type ProviderRequest struct {
	Model     string
	System    string
	Messages  []contracts.Message
	Tools     []contracts.ToolSchema
	MaxTokens int
}

// ProviderResponse is a normalized reply.
type ProviderResponse struct {
	// ID is the provider's identifier for this reply. The router keys the
	// cost event on it, so it must be unique per call.
	ID string

	Text       string
	ToolCalls  []contracts.ToolCall
	StopReason contracts.StopReason

	// InputTokens excludes cache traffic, which vendors bill at different
	// rates and report separately.
	InputTokens      int
	CacheWriteTokens int
	CacheReadTokens  int
	OutputTokens     int
}

// totalTokens is everything the call consumed, which is what budgets and
// rate limits count.
func (r ProviderResponse) totalTokens() int {
	return r.InputTokens + r.CacheWriteTokens + r.CacheReadTokens + r.OutputTokens
}

// Errors adapters wrap so the router can decide what to do without knowing
// the vendor. Only ErrUnavailable moves the router on to a fallback model;
// the rest are returned to the caller.
var (
	// ErrUnavailable means the provider could not serve the request right
	// now: a 5xx, an overload response, or a transport failure.
	ErrUnavailable = errors.New("provider unavailable")

	// ErrRateLimited means the provider refused for rate. Temporal's
	// activity retry handles it; falling back would hide the limit.
	ErrRateLimited = errors.New("provider rate limited")

	// ErrAuth means the provider rejected the credential.
	ErrAuth = errors.New("provider rejected credentials")
)
