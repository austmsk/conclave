package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/anthropic"
)

const canaryKey = "canary_anthropic_key_7f3a9c"

// server answers every request with status and body, and hands the decoded
// request body and headers to inspect.
func server(t *testing.T, status int, body string, inspect func(http.Header, []byte)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if inspect != nil {
			inspect(r.Header, raw)
		}
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			t.Errorf("request to %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func client(t *testing.T, url string) *anthropic.Client {
	t.Helper()
	c, err := anthropic.New(canaryKey, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// conversation is one tool round: a call that succeeded and one refused.
func conversation() llm.ProviderRequest {
	return llm.ProviderRequest{
		Model:  "claude-fable-5-1",
		System: "You are the implementer.",
		Messages: []contracts.Message{
			{Role: contracts.MessageUser, Text: "Fix issue #12"},
			{Role: contracts.MessageAssistant, Text: "Reading.", ToolCalls: []contracts.ToolCall{
				{ID: "toolu_1", Name: contracts.ToolReadFile, Arguments: json.RawMessage(`{"path":"a.go"}`)},
				{ID: "toolu_2", Name: contracts.ToolRunTests},
			}},
			{Role: contracts.MessageUser, ToolResults: []contracts.ToolResult{
				{CallID: "toolu_1", Output: "package a"},
				{CallID: "toolu_2", Output: "partial", Error: "tests failed"},
			}},
		},
		Tools: []contracts.ToolSchema{
			{Name: contracts.ToolReadFile, Description: "Read a file.", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		MaxTokens: 1024,
	}
}

func TestRequestWireFormat(t *testing.T) {
	want, err := os.ReadFile("testdata/request.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var gotBody []byte
	var gotHeader http.Header
	srv := server(t, http.StatusOK, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`,
		func(h http.Header, b []byte) { gotHeader, gotBody = h, b })

	if _, err := client(t, srv.URL).Complete(context.Background(), conversation()); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, gotBody, want) {
		t.Fatalf("request body\n%s\nwant\n%s", gotBody, want)
	}
	if gotHeader.Get("x-api-key") != canaryKey || gotHeader.Get("anthropic-version") == "" {
		t.Fatalf("headers %v", gotHeader)
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("decoding %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("decoding %s: %v", b, err)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

func TestResponseDecoding(t *testing.T) {
	body := `{
	  "content": [
	    {"type": "text", "text": "Running "},
	    {"type": "text", "text": "both."},
	    {"type": "tool_use", "id": "toolu_9", "name": "run_tests", "input": {}},
	    {"type": "tool_use", "id": "toolu_10", "name": "read_file", "input": {"path": "b.go"}}
	  ],
	  "stop_reason": "tool_use",
	  "usage": {"input_tokens": 100, "output_tokens": 20, "cache_creation_input_tokens": 5, "cache_read_input_tokens": 7}
	}`
	srv := server(t, http.StatusOK, body, nil)
	resp, err := client(t, srv.URL).Complete(context.Background(), conversation())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "Running both." || resp.StopReason != contracts.StopToolUse {
		t.Fatalf("text %q stop %q", resp.Text, resp.StopReason)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "toolu_9" || resp.ToolCalls[1].Name != contracts.ToolReadFile ||
		string(resp.ToolCalls[1].Arguments) != `{"path": "b.go"}` {
		t.Fatalf("tool calls %+v", resp.ToolCalls)
	}
	if resp.InputTokens != 112 || resp.OutputTokens != 20 {
		t.Fatalf("tokens %d/%d, want 112/20", resp.InputTokens, resp.OutputTokens)
	}
}

func TestStopReasons(t *testing.T) {
	cases := []struct {
		wire string
		want contracts.StopReason
		ok   bool
	}{
		{"end_turn", contracts.StopEndTurn, true},
		{"stop_sequence", contracts.StopEndTurn, true},
		{"max_tokens", contracts.StopMaxTokens, true},
		{"refusal", "", false},
		{"pause_turn", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			body := `{"content":[{"type":"text","text":"x"}],"stop_reason":"` + tc.wire + `","usage":{}}`
			resp, err := client(t, server(t, http.StatusOK, body, nil).URL).Complete(context.Background(), conversation())
			if tc.ok != (err == nil) || resp.StopReason != tc.want {
				t.Fatalf("stop %q err %v", resp.StopReason, err)
			}
		})
	}
}

func TestMalformedResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"tool_use with no calls", `{"content":[{"type":"text","text":"x"}],"stop_reason":"tool_use","usage":{}}`},
		{"unknown block", `{"content":[{"type":"image"}],"stop_reason":"end_turn","usage":{}}`},
		{"not JSON", `<html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client(t, server(t, http.StatusOK, tc.body, nil).URL).Complete(context.Background(), conversation())
			if err == nil {
				t.Fatal("accepted a malformed response")
			}
		})
	}
}

// TestStatusMapping also attempts to leak the key: every error body echoes
// it, and no error string may contain it.
func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusTooManyRequests, llm.ErrRateLimited},
		{http.StatusUnauthorized, llm.ErrAuth},
		{http.StatusForbidden, llm.ErrAuth},
		{http.StatusInternalServerError, llm.ErrUnavailable},
		{http.StatusServiceUnavailable, llm.ErrUnavailable},
		{529, llm.ErrUnavailable},
		{http.StatusBadRequest, nil},
		{http.StatusRequestEntityTooLarge, nil},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			body := `{"type":"error","error":{"type":"some_error","message":"bad key ` + canaryKey + `"}}`
			_, err := client(t, server(t, tc.status, body, nil).URL).Complete(context.Background(), conversation())
			assertStatusError(t, err, tc.want)
			if strings.Contains(err.Error(), canaryKey) {
				t.Fatalf("error leaks the key: %v", err)
			}
			if !strings.Contains(err.Error(), "some_error") {
				t.Fatalf("error lost the API's error type: %v", err)
			}
		})
	}
}

// assertStatusError checks err is want, or, when want is nil, that it is
// none of the typed errors the router acts on.
func assertStatusError(t *testing.T, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	if want != nil {
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
		return
	}
	for _, typed := range []error{llm.ErrRateLimited, llm.ErrAuth, llm.ErrUnavailable} {
		if errors.Is(err, typed) {
			t.Fatalf("mapped to %v: %v", typed, err)
		}
	}
}

func TestTransportFailureIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := client(t, url).Complete(context.Background(), conversation())
	if !errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), canaryKey) {
		t.Fatalf("error leaks the key: %v", err)
	}
}

func TestCancelledContextIsNotUnavailable(t *testing.T) {
	srv := server(t, http.StatusOK, `{}`, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client(t, srv.URL).Complete(ctx, conversation())
	if !errors.Is(err, context.Canceled) || errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("got %v, want context.Canceled and not ErrUnavailable", err)
	}
}

func TestNewRequiresKey(t *testing.T) {
	if _, err := anthropic.New("", "", nil); err == nil {
		t.Fatal("New accepted an empty key")
	}
}
