package faketest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
)

// Errors reported when a request does not match the recording.
var (
	ErrExhausted         = errors.New("fake provider: script has no more turns")
	ErrUnexpectedRequest = errors.New("fake provider: request does not match the recording")
	ErrRejected          = fmt.Errorf("fake provider: scripted rejected reply: %w", llm.ErrUnsupportedReply)
)

// Script is a recorded conversation, as stored in a fixture file.
type Script struct {
	Turns []Turn `json:"turns"`
}

// Turn is one request and the reply it receives.
type Turn struct {
	Expect   Expect   `json:"expect"`
	Response Response `json:"response"`

	// Error, when set to "unavailable" or "rate_limited", makes the turn
	// fail with the matching llm error instead of replying. "rejected"
	// stands for a reply the provider billed and the adapter refused: the
	// turn fails with ErrRejected, which wraps llm.ErrUnsupportedReply, and returns the response's ID and token
	// counts, as the llm.Provider contract requires.
	Error string `json:"error,omitempty"`
}

// Expect constrains the request a turn answers. Zero values are not checked.
type Expect struct {
	Model    string `json:"model,omitempty"`
	Messages int    `json:"messages,omitempty"`
}

// Response is the recorded reply. A reply without an ID gets a unique one,
// as a real provider's would have.
type Response struct {
	ID           string               `json:"id,omitempty"`
	Text         string               `json:"text,omitempty"`
	ToolCalls    []contracts.ToolCall `json:"tool_calls,omitempty"`
	StopReason   contracts.StopReason `json:"stop_reason"`
	InputTokens  int                  `json:"input_tokens"`
	OutputTokens int                  `json:"output_tokens"`

	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
}

// replyIDs numbers generated reply IDs across every fake in the process, so
// no two fakes ever issue the same ID and cost events never collide by
// accident. The numbers depend on test order: a test that asserts on cost
// keys must set Response.ID itself.
var replyIDs atomic.Int64

// Provider replays a Script. It is safe for concurrent use, though a
// recording only makes sense for one conversation at a time.
type Provider struct {
	mu       sync.Mutex
	turns    []Turn
	next     int
	requests []llm.ProviderRequest
}

var _ llm.Provider = (*Provider)(nil)

// New returns a provider that replays turns in order.
func New(turns ...Turn) *Provider {
	return &Provider{turns: turns}
}

// Load reads a Script from a JSON fixture. Unknown fields are an error.
func Load(path string) (*Provider, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening fixture %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var s Script
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decoding fixture %s: %w", path, err)
	}
	return New(s.Turns...), nil
}

// Complete serves the next recorded turn after checking the request.
func (p *Provider) Complete(ctx context.Context, req llm.ProviderRequest) (llm.ProviderResponse, error) {
	if err := ctx.Err(); err != nil {
		return llm.ProviderResponse{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.next >= len(p.turns) {
		return llm.ProviderResponse{}, fmt.Errorf("request %d: %w", len(p.requests), ErrExhausted)
	}
	turn := p.turns[p.next]
	if err := p.check(turn.Expect, req); err != nil {
		return llm.ProviderResponse{}, fmt.Errorf("turn %d: %w: %w", p.next+1, ErrUnexpectedRequest, err)
	}
	p.next++
	switch turn.Error {
	case "":
		return reply(turn.Response), nil
	case "unavailable":
		return llm.ProviderResponse{}, fmt.Errorf("fake: %w", llm.ErrUnavailable)
	case "rate_limited":
		return llm.ProviderResponse{}, fmt.Errorf("fake: %w", llm.ErrRateLimited)
	case "rejected":
		r := reply(turn.Response)
		billed := llm.ProviderResponse{
			ID: r.ID, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			CacheWriteTokens: r.CacheWriteTokens, CacheReadTokens: r.CacheReadTokens,
		}
		return billed, ErrRejected
	default:
		return llm.ProviderResponse{}, fmt.Errorf("fake: unknown scripted error %q", turn.Error)
	}
}

func reply(r Response) llm.ProviderResponse {
	id := r.ID
	if id == "" {
		id = fmt.Sprintf("fake_msg_%d", replyIDs.Add(1))
	}
	return llm.ProviderResponse{
		ID:               id,
		Text:             r.Text,
		ToolCalls:        r.ToolCalls,
		StopReason:       r.StopReason,
		InputTokens:      r.InputTokens,
		CacheWriteTokens: r.CacheWriteTokens,
		CacheReadTokens:  r.CacheReadTokens,
		OutputTokens:     r.OutputTokens,
	}
}

// check compares req with the turn's expectations and, when the previous
// served turn asked for tools, requires the request to carry that call and
// a result for every one of its IDs. Callers hold p.mu.
func (p *Provider) check(want Expect, req llm.ProviderRequest) error {
	if want.Model != "" && req.Model != want.Model {
		return fmt.Errorf("model %q, want %q", req.Model, want.Model)
	}
	if want.Messages != 0 && len(req.Messages) != want.Messages {
		return fmt.Errorf("%d messages, want %d", len(req.Messages), want.Messages)
	}
	if p.next == 0 {
		return nil
	}
	prev := p.turns[p.next-1]
	if prev.Error != "" || len(prev.Response.ToolCalls) == 0 {
		return nil
	}
	return checkToolRound(prev.Response.ToolCalls, req.Messages)
}

// checkToolRound requires messages to end with the assistant turn that made
// calls, followed by a user turn answering each call exactly once.
func checkToolRound(calls []contracts.ToolCall, msgs []contracts.Message) error {
	if len(msgs) < 2 {
		return errors.New("previous turn called tools but the request has no tool round")
	}
	asked, answered := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if asked.Role != contracts.MessageAssistant || answered.Role != contracts.MessageUser {
		return errors.New("tool round is not an assistant turn followed by a user turn")
	}
	want := callIDs(calls)
	if got := callIDs(asked.ToolCalls); !slices.Equal(got, want) {
		return fmt.Errorf("assistant turn replays calls %v, want %v", got, want)
	}
	got := make([]string, 0, len(answered.ToolResults))
	for _, r := range answered.ToolResults {
		got = append(got, r.CallID)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		return fmt.Errorf("tool results answer %v, want %v", got, want)
	}
	return nil
}

func callIDs(calls []contracts.ToolCall) []string {
	ids := make([]string, 0, len(calls))
	for _, c := range calls {
		ids = append(ids, c.ID)
	}
	slices.Sort(ids)
	return ids
}

// Requests returns a copy of every request received, in order.
func (p *Provider) Requests() []llm.ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

// Remaining reports how many recorded turns have not been served.
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.turns) - p.next
}
