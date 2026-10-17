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
	"time"

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
				{ID: "toolu_3", Name: contracts.ToolWriteFile, Arguments: json.RawMessage(`{"path":"b.go"}`)},
			}},
			{Role: contracts.MessageUser, ToolResults: []contracts.ToolResult{
				{CallID: "toolu_1", Output: "package a"},
				{CallID: "toolu_2", Output: "partial", Error: "tests failed"},
				{CallID: "toolu_3"},
			}},
		},
		Tools: []contracts.ToolSchema{
			{Name: contracts.ToolReadFile, Description: "Read a file.", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Name: contracts.ToolRunTests, Description: "Run the tests."},
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
	srv := server(t, http.StatusOK, `{"id":"msg_1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`,
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
	  "id": "msg_01XYZ",
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
	if resp.ID != "msg_01XYZ" {
		t.Fatalf("ID %q", resp.ID)
	}
	if resp.InputTokens != 100 || resp.CacheWriteTokens != 5 || resp.CacheReadTokens != 7 || resp.OutputTokens != 20 {
		t.Fatalf("tokens %+v", resp)
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
			body := `{"id":"msg_1","content":[{"type":"text","text":"x"}],"stop_reason":"` + tc.wire + `","usage":{}}`
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
		{"tool_use with no calls", `{"id":"msg_1","content":[{"type":"text","text":"x"}],"stop_reason":"tool_use","usage":{}}`},
		{"unknown block", `{"id":"msg_1","content":[{"type":"image"}],"stop_reason":"end_turn","usage":{}}`},
		{"no id", `{"content":[{"type":"text","text":"x"}],"stop_reason":"end_turn","usage":{}}`},
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

// TestRejectedReplyReportsUsage: a reply the API billed and the adapter
// refuses still reports its ID and tokens, so the router can charge them.
func TestRejectedReplyReportsUsage(t *testing.T) {
	const usage = `"usage":{"input_tokens":120,"output_tokens":30,"cache_creation_input_tokens":5,"cache_read_input_tokens":7}`
	cases := []struct {
		name, body, wantID string
		want               error
	}{
		{"refusal", `{"id":"msg_r","content":[{"type":"text","text":"no"}],"stop_reason":"refusal",` + usage + `}`, "msg_r", llm.ErrUnsupportedReply},
		{"unknown block", `{"id":"msg_u","content":[{"type":"image"}],"stop_reason":"end_turn",` + usage + `}`, "msg_u", llm.ErrUnsupportedReply},
		{"tool_use with no calls", `{"id":"msg_t","content":[],"stop_reason":"tool_use",` + usage + `}`, "msg_t", llm.ErrMalformedReply},
		{"no id", `{"content":[{"type":"text","text":"x"}],"stop_reason":"end_turn",` + usage + `}`, "", llm.ErrMalformedReply},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client(t, server(t, http.StatusOK, tc.body, nil).URL).Complete(context.Background(), conversation())
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if resp.ID != tc.wantID || resp.InputTokens != 120 || resp.OutputTokens != 30 ||
				resp.CacheWriteTokens != 5 || resp.CacheReadTokens != 7 {
				t.Fatalf("billed %+v", resp)
			}
			if resp.Text != "" || resp.ToolCalls != nil || resp.StopReason != "" {
				t.Fatalf("a rejected reply carried content: %+v", resp)
			}
		})
	}
}

// TestNegativeUsageIsMalformed: a reply reporting a negative token count is
// refused as malformed. The negative count is dropped, and the counts that
// are not negative are still reported, so the router charges them.
func TestNegativeUsageIsMalformed(t *testing.T) {
	cases := []struct {
		name  string
		usage string
		want  llm.ProviderResponse
	}{
		{"input", `{"input_tokens":-120,"output_tokens":30}`, llm.ProviderResponse{ID: "msg_n", OutputTokens: 30}},
		{"output", `{"input_tokens":120,"output_tokens":-30}`, llm.ProviderResponse{ID: "msg_n", InputTokens: 120}},
		{"cache write", `{"input_tokens":120,"output_tokens":30,"cache_creation_input_tokens":-5}`,
			llm.ProviderResponse{ID: "msg_n", InputTokens: 120, OutputTokens: 30}},
		{"cache read", `{"input_tokens":120,"output_tokens":30,"cache_read_input_tokens":-7}`,
			llm.ProviderResponse{ID: "msg_n", InputTokens: 120, OutputTokens: 30}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"msg_n","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn","usage":` + tc.usage + `}`
			resp, err := client(t, server(t, http.StatusOK, body, nil).URL).Complete(context.Background(), conversation())
			if !errors.Is(err, llm.ErrMalformedReply) {
				t.Fatalf("got %v, want ErrMalformedReply", err)
			}
			if resp.ID != tc.want.ID || resp.InputTokens != tc.want.InputTokens || resp.OutputTokens != tc.want.OutputTokens ||
				resp.CacheWriteTokens != tc.want.CacheWriteTokens || resp.CacheReadTokens != tc.want.CacheReadTokens {
				t.Fatalf("billed %+v, want %+v", resp, tc.want)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"30", 30 * time.Second},
		{"", 0},
		{"soon", 0},
		{"-5", 0},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.header != "" {
					w.Header().Set("retry-after", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			}))
			t.Cleanup(srv.Close)
			_, err := client(t, srv.URL).Complete(context.Background(), conversation())
			if !errors.Is(err, llm.ErrRateLimited) {
				t.Fatalf("got %v, want ErrRateLimited", err)
			}
			var rl *llm.RateLimitError
			got := time.Duration(0)
			if errors.As(err, &rl) {
				got = rl.RetryAfter
			}
			if got != tc.want {
				t.Fatalf("RetryAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEmptyMessageIsRefused: a message with no content is caught before it
// is sent, rather than spending a request on a certain 400.
func TestEmptyMessageIsRefused(t *testing.T) {
	sent := false
	srv := server(t, http.StatusOK, `{}`, func(http.Header, []byte) { sent = true })
	req := conversation()
	req.Messages = append(req.Messages, contracts.Message{Role: contracts.MessageAssistant})
	if _, err := client(t, srv.URL).Complete(context.Background(), req); err == nil {
		t.Fatal("accepted an empty message")
	}
	if sent {
		t.Fatal("the empty message was sent")
	}
}

// TestErrorMessageIsDropped: a validation error quoting the request
// keeps its status and type but none of its message, so repository content
// never reaches error strings, history, or traces.
func TestErrorMessageIsDropped(t *testing.T) {
	const secret = "canary_repo_content_4b1d"
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"bad field near ` + secret + `"}}`
	_, err := client(t, server(t, http.StatusBadRequest, body, nil).URL).Complete(context.Background(), conversation())
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "bad field") {
		t.Fatalf("error carries the message: %v", err)
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_request_error") {
		t.Fatalf("error lost its status or type: %v", err)
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
		{http.StatusRequestTimeout, llm.ErrUnavailable},
		{http.StatusBadRequest, llm.ErrInvalidRequest},
		{http.StatusNotFound, llm.ErrInvalidRequest},
		{http.StatusRequestEntityTooLarge, llm.ErrInvalidRequest},
		{http.StatusMultipleChoices, llm.ErrUnsupportedReply},
		{http.StatusTemporaryRedirect, llm.ErrUnsupportedReply},
		{http.StatusAccepted, nil},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			body := `{"type":"error","error":{"type":"some_error ` + canaryKey + `","message":"bad key ` + canaryKey + `"}}`
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
	for _, typed := range []error{llm.ErrRateLimited, llm.ErrAuth, llm.ErrUnavailable, llm.ErrInvalidRequest} {
		if errors.Is(err, typed) {
			t.Fatalf("mapped to %v: %v", typed, err)
		}
	}
}

// TestRedirectIsNotFollowed attempts to steal the key with a redirect to
// another host. Go copies x-api-key across such a redirect, so the attacker
// would receive it if the client followed one; it must not, whether New
// built the HTTP client or the caller supplied it.
func TestRedirectIsNotFollowed(t *testing.T) {
	statuses := []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	}
	clients := []struct {
		name string
		http func() *http.Client
	}{
		{"default client", func() *http.Client { return nil }},
		{"caller's client", func() *http.Client { return &http.Client{Timeout: time.Minute} }},
	}
	for _, status := range statuses {
		for _, hc := range clients {
			t.Run(http.StatusText(status)+"/"+hc.name, func(t *testing.T) {
				attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					t.Errorf("attacker received a request with x-api-key %q", r.Header.Get("x-api-key"))
				}))
				t.Cleanup(attacker.Close)
				redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, attacker.URL+"/v1/messages", status)
				}))
				t.Cleanup(redirector.Close)

				caller := hc.http()
				c, err := anthropic.New(canaryKey, redirector.URL, caller)
				if err != nil {
					t.Fatal(err)
				}
				_, err = c.Complete(context.Background(), conversation())
				if !errors.Is(err, llm.ErrUnsupportedReply) || !llm.IsPermanent(err) {
					t.Fatalf("got %v, want permanent ErrUnsupportedReply", err)
				}
				if caller != nil && caller.CheckRedirect != nil {
					t.Fatal("New changed the caller's client")
				}
			})
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

func TestBaseURLMustBeHTTPS(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{"", true},
		{"https://api.anthropic.com", true},
		{"http://127.0.0.1:8080", true},
		{"http://[::1]:8080", true},
		{"http://localhost:8080", true},
		{"http://api.anthropic.com", false},
		{"http://10.0.0.5", false},
		{"ftp://api.anthropic.com", false},
		{"api.anthropic.com", false},
		{"https://", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			_, err := anthropic.New(canaryKey, tc.url, nil)
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), canaryKey) {
				t.Fatalf("error leaks the key: %v", err)
			}
		})
	}
}

func TestNewRequiresKey(t *testing.T) {
	if _, err := anthropic.New("", "", nil); err == nil {
		t.Fatal("New accepted an empty key")
	}
}
