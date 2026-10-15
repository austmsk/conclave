package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
)

const (
	// DefaultBaseURL is the public API endpoint.
	DefaultBaseURL = "https://api.anthropic.com"

	apiVersion = "2023-06-01"

	// maxErrorBody bounds how much of an error response is read.
	maxErrorBody = 4 << 10
)

// Client is an llm.Provider for Anthropic.
type Client struct {
	key     string
	baseURL string
	http    *http.Client
}

var _ llm.Provider = (*Client)(nil)

// New returns a client. baseURL may be empty for DefaultBaseURL. It must be
// https, except on a loopback host, where tests run an httptest server: the
// key travels in a header, and plain http to anywhere else would send it in
// the clear. httpClient may be nil for http.DefaultClient.
func New(apiKey, baseURL string, httpClient *http.Client) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("anthropic: no API key")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if err := checkBaseURL(baseURL); err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{key: apiKey, baseURL: baseURL, http: httpClient}, nil
}

func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("anthropic: parsing base URL: %w", err)
	}
	if u.Scheme == "https" && u.Host != "" {
		return nil
	}
	if u.Scheme == "http" && isLoopback(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("anthropic: base URL %q must be https", u.Redacted())
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Complete sends one Messages API request.
func (c *Client) Complete(ctx context.Context, req llm.ProviderRequest) (llm.ProviderResponse, error) {
	body, err := json.Marshal(encodeRequest(req))
	if err != nil {
		return llm.ProviderResponse{}, fmt.Errorf("anthropic: encoding request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return llm.ProviderResponse{}, fmt.Errorf("anthropic: building request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", apiVersion)
	httpReq.Header.Set("x-api-key", c.key)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return llm.ProviderResponse{}, fmt.Errorf("anthropic: %w", ctx.Err())
		}
		// A transport error names the URL but never the headers, so the key
		// cannot leak through it; the cause is still dropped to be sure.
		return llm.ProviderResponse{}, fmt.Errorf("anthropic: transport failure: %w", llm.ErrUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return llm.ProviderResponse{}, c.statusError(resp)
	}
	var wire response
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return llm.ProviderResponse{}, fmt.Errorf("anthropic: decoding response: %w", err)
	}
	return decodeResponse(wire)
}

// statusError maps an HTTP failure to a typed error. Only the API's own
// error type and message are kept from the body, and the key is redacted
// from them in case a server ever echoes it.
func (c *Client) statusError(resp *http.Response) error {
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = json.Unmarshal(raw, &body)
	detail := fmt.Sprintf("anthropic: HTTP %d %s: %s", resp.StatusCode, body.Error.Type, body.Error.Message)
	detail = strings.ReplaceAll(detail, c.key, "[redacted]")

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", detail, llm.ErrRateLimited)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s: %w", detail, llm.ErrAuth)
	case resp.StatusCode >= 500:
		// Includes 529 overloaded.
		return fmt.Errorf("%s: %w", detail, llm.ErrUnavailable)
	default:
		return errors.New(detail)
	}
}

// Wire types for the Messages API.

type request struct {
	Model     string    `json:"model"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Tools     []tool    `json:"tools,omitempty"`
	MaxTokens int       `json:"max_tokens"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type block struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type response struct {
	ID         string  `json:"id"`
	Content    []block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func encodeRequest(req llm.ProviderRequest) request {
	out := request{Model: req.Model, System: req.System, MaxTokens: req.MaxTokens}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, encodeMessage(m))
	}
	for _, t := range req.Tools {
		schema := t.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			// The API requires an object schema; a tool without
			// parameters takes an empty object.
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, tool{Name: string(t.Name), Description: t.Description, InputSchema: schema})
	}
	return out
}

// encodeMessage puts tool results first in a user turn, as the API requires,
// and text before tool calls in an assistant turn.
func encodeMessage(m contracts.Message) message {
	out := message{Role: string(m.Role), Content: []block{}}
	for _, r := range m.ToolResults {
		out.Content = append(out.Content, toolResultBlock(r))
	}
	if m.Text != "" {
		out.Content = append(out.Content, block{Type: "text", Text: m.Text})
	}
	for _, c := range m.ToolCalls {
		input := c.Arguments
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		out.Content = append(out.Content, block{Type: "tool_use", ID: c.ID, Name: string(c.Name), Input: input})
	}
	return out
}

// toolResultBlock reports a refusal or failure as an error result, with the
// error first so the model sees why before any partial output. A tool that
// succeeds silently sends no content, which the API accepts.
func toolResultBlock(r contracts.ToolResult) block {
	b := block{Type: "tool_result", ToolUseID: r.CallID, Content: r.Output}
	if r.Error != "" {
		b.IsError = true
		b.Content = r.Error
		if r.Output != "" {
			b.Content += "\n" + r.Output
		}
	}
	return b
}

func decodeResponse(w response) (llm.ProviderResponse, error) {
	stop, err := stopReason(w.StopReason)
	if err != nil {
		return llm.ProviderResponse{}, err
	}
	out := llm.ProviderResponse{
		ID:               w.ID,
		StopReason:       stop,
		InputTokens:      w.Usage.InputTokens,
		CacheWriteTokens: w.Usage.CacheCreationInputTokens,
		CacheReadTokens:  w.Usage.CacheReadInputTokens,
		OutputTokens:     w.Usage.OutputTokens,
	}
	for _, b := range w.Content {
		switch b.Type {
		case "text":
			out.Text += b.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, contracts.ToolCall{ID: b.ID, Name: contracts.ToolName(b.Name), Arguments: b.Input})
		case "thinking", "redacted_thinking":
			// Not surfaced; extended thinking is not requested.
		default:
			return llm.ProviderResponse{}, fmt.Errorf("anthropic: unknown content block type %q", b.Type)
		}
	}
	if stop == contracts.StopToolUse && len(out.ToolCalls) == 0 {
		return llm.ProviderResponse{}, errors.New("anthropic: stop reason tool_use with no tool calls")
	}
	return out, nil
}

// stopReason fails on values it does not know, so a new API behaviour shows
// up as an error rather than as an agent that stops for no visible reason.
func stopReason(s string) (contracts.StopReason, error) {
	switch s {
	case "end_turn", "stop_sequence":
		return contracts.StopEndTurn, nil
	case "tool_use":
		return contracts.StopToolUse, nil
	case "max_tokens":
		return contracts.StopMaxTokens, nil
	default:
		return "", fmt.Errorf("anthropic: unsupported stop reason %q", s)
	}
}
