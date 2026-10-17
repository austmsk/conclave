package llm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
)

// Provider is one model vendor's API, normalized. Adapters translate
// ProviderRequest into the vendor's wire format and the reply back into
// ProviderResponse; nothing outside an adapter knows the wire format (FR-I2).
//
// A ProviderRequest carries no repository identity. The tier check happens in
// the router before an adapter is called, so an adapter cannot be the place
// where it is forgotten.
//
// When the provider replied but the adapter rejects the reply, for example
// an unsupported stop reason, Complete returns the error together with a
// ProviderResponse carrying the reply ID and token counts. The provider has
// billed those tokens, and the router charges them to the budget and the
// ledger whether or not the call succeeded.
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
	// activity retry handles it; falling back would hide the limit. Adapters
	// that learn how long to wait return a *RateLimitError wrapping it.
	ErrRateLimited = errors.New("provider rate limited")

	// ErrAuth means the provider rejected the credential.
	ErrAuth = errors.New("provider rejected credentials")

	// ErrUnsupportedReply means the provider replied, and billed, in a
	// shape the adapter does not handle: a stop reason or content block it
	// does not know, a refusal among them. That is a property of the API
	// or the request, so a retry would pay for another reply of the same
	// shape; it is permanent until the adapter is updated. A redirect, which
	// adapters never follow, is reported the same way: the endpoint is
	// misconfigured, and asking it again sends the request to the same
	// place.
	ErrUnsupportedReply = errors.New("provider reply is not supported")

	// ErrMalformedReply means a reply the adapter knows the shape of came
	// back broken: no ID, a negative token count, or a tool_use stop with no
	// tool calls. That is a one-off fault, so it is retryable, and the
	// budget bounds the cost of retrying.
	ErrMalformedReply = errors.New("provider reply is malformed")
)

// RateLimitError is a rate-limit refusal that says how long to wait. The
// activity layer uses RetryAfter as the next retry interval.
type RateLimitError struct {
	RetryAfter time.Duration
	Detail     string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s: retry after %s: %v", e.Detail, e.RetryAfter, ErrRateLimited)
}

// Unwrap lets errors.Is match ErrRateLimited.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }
