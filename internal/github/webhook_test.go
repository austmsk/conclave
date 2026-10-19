package github_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1" // only to forge the legacy header GitHub also sends
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
)

const (
	sigHeader      = "X-Hub-Signature-256"
	deliveryHeader = "X-GitHub-Delivery"
	secret         = githubtest.CanaryWebhookSecret
)

// syncBuffer is a log sink the handler and the dispatcher's goroutine can
// write to at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness is a Receiver wired to a Dispatcher and a FakeStarter, logging
// JSON at debug level into logs.
type harness struct {
	starter *githubtest.FakeStarter
	disp    *github.Dispatcher
	recv    *github.Receiver
	logs    *syncBuffer
	// ran is closed when Run returns; nil until start.
	ran chan struct{}
}

type options struct {
	enabled map[string]bool
	depth   int
	// noRun leaves the dispatcher stopped until the test calls start.
	noRun   bool
	starter *githubtest.FakeStarter
}

func newHarness(t *testing.T, o options) *harness {
	t.Helper()
	if o.enabled == nil {
		o.enabled = githubtest.EnabledRepos()
	}
	if o.depth == 0 {
		o.depth = 16
	}
	if o.starter == nil {
		o.starter = &githubtest.FakeStarter{}
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	disp := github.NewDispatcher(o.starter, log, o.depth)
	recv, err := github.NewReceiver(githubtest.WebhookConfig(), o.enabled, []byte(secret), disp, log)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{starter: o.starter, disp: disp, recv: recv, logs: logs}
	if !o.noRun {
		h.start(t)
	}
	t.Cleanup(func() { h.settle(t) })
	return h
}

func (h *harness) start(t *testing.T) {
	t.Helper()
	h.ran = make(chan struct{})
	go func() {
		defer close(h.ran)
		h.disp.Run(t.Context())
	}()
}

// settle closes the dispatcher and waits for Run to return, so every
// starter call that will happen has happened.
func (h *harness) settle(t *testing.T) {
	t.Helper()
	if h.ran == nil {
		h.disp.Close(0)
		return
	}
	h.disp.Close(5 * time.Second)
	select {
	case <-h.ran:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher Run did not return after Close")
	}
}

func (h *harness) send(t *testing.T, event string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return h.serve(githubtest.Request(t, secret, event, body))
}

// sendAs sends a delivery with its own delivery ID.
func (h *harness) sendAs(t *testing.T, delivery, event string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := githubtest.Request(t, secret, event, body)
	req.Header.Set(deliveryHeader, delivery)
	return h.serve(req)
}

func (h *harness) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.recv.ServeHTTP(rec, req)
	return rec
}

// records returns the log lines whose key holds value.
func (h *harness) records(t *testing.T, key, value string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if s, ok := rec[key].(string); ok && s == value {
			out = append(out, rec)
		}
	}
	return out
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}

func wantNoCalls(t *testing.T, h *harness) {
	t.Helper()
	h.settle(t)
	if calls := h.starter.Calls(); len(calls) != 0 {
		t.Fatalf("starter got %d calls, want none", len(calls))
	}
}

// forgery returns a delivery of body that must fail verification.
type forgery func(t *testing.T, body []byte) *http.Request

func signatureForgeries() map[string]forgery {
	valid := func(body []byte) string { return githubtest.Sign([]byte(secret), body) }
	withSig := func(sig func(body []byte) string) forgery {
		return func(t *testing.T, body []byte) *http.Request {
			req := githubtest.Request(t, secret, "issues", body)
			req.Header.Set(sigHeader, sig(body))
			return req
		}
	}
	return map[string]forgery{
		"missing header": func(t *testing.T, body []byte) *http.Request {
			req := githubtest.Request(t, secret, "issues", body)
			req.Header.Del(sigHeader)
			return req
		},
		"SHA-1 header only": func(t *testing.T, body []byte) *http.Request {
			req := githubtest.Request(t, secret, "issues", body)
			req.Header.Del(sigHeader)
			mac := hmac.New(sha1.New, []byte(secret))
			_, _ = mac.Write(body)
			req.Header.Set("X-Hub-Signature", "sha1="+hex.EncodeToString(mac.Sum(nil)))
			return req
		},
		"wrong prefix": withSig(func(b []byte) string { return "sha512=" + strings.TrimPrefix(valid(b), "sha256=") }),
		"non-hex":      withSig(func([]byte) string { return "sha256=" + strings.Repeat("zz", 32) }),
		"63 hex chars": withSig(func(b []byte) string { s := valid(b); return s[:len(s)-1] }),
		"65 hex chars": withSig(func(b []byte) string { return valid(b) + "0" }),
		"wrong secret": withSig(func(b []byte) string { return githubtest.Sign([]byte("canary_other_webhook_secret_0123456789ab"), b) }),
		"two signatures": func(t *testing.T, body []byte) *http.Request {
			req := githubtest.Request(t, secret, "issues", body)
			req.Header.Add(sigHeader, valid(body))
			return req
		},
		"one body byte flipped": func(t *testing.T, body []byte) *http.Request {
			flipped := bytes.Clone(body)
			flipped[len(flipped)/2] ^= 0x01
			req := githubtest.Request(t, secret, "issues", flipped)
			req.Header.Set(sigHeader, valid(body))
			return req
		},
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	body := githubtest.LabeledEvent().JSON()
	for name, forge := range signatureForgeries() {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, options{})
			req := forge(t, body)
			sent := req.Header.Get(sigHeader)

			rec := h.serve(req)

			wantStatus(t, rec, http.StatusUnauthorized)
			if got := rec.Body.String(); got != "invalid signature\n" {
				t.Errorf("response body %q, want the fixed text", got)
			}
			wantNoCalls(t, h)
			logs := h.logs.String()
			if strings.Contains(logs, secret) {
				t.Error("log carries the webhook secret")
			}
			if sent != "" && strings.Contains(logs, strings.TrimPrefix(sent, "sha256=")) {
				t.Error("log carries the signature")
			}
		})
	}
}

func TestWebhookVerifiesBeforeParsing(t *testing.T) {
	malformed := []byte(`{"action": "labeled", "repository": `)
	tests := []struct {
		name string
		sign bool
		want int
	}{
		{"bad signature on malformed JSON is unauthorized, not bad request", false, http.StatusUnauthorized},
		{"good signature on malformed JSON is a bad request", true, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, options{})
			req := githubtest.Request(t, secret, "issues", malformed)
			if !tt.sign {
				req.Header.Set(sigHeader, githubtest.Sign([]byte("canary_wrong"), malformed))
			}
			wantStatus(t, h.serve(req), tt.want)
			wantNoCalls(t, h)
		})
	}
}

func TestNewReceiverRejectsWeakSecret(t *testing.T) {
	tests := []struct {
		name    string
		secret  []byte
		wantErr bool
	}{
		{"empty", nil, true},
		{"one byte short", []byte("canary_" + strings.Repeat("x", github.MinWebhookSecretLen-8)), true},
		{"exactly the minimum", []byte("canary_" + strings.Repeat("x", github.MinWebhookSecretLen-7)), false},
	}
	log := slog.New(slog.DiscardHandler)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disp := github.NewDispatcher(&githubtest.FakeStarter{}, log, 1)
			_, err := github.NewReceiver(githubtest.WebhookConfig(), githubtest.EnabledRepos(), tt.secret, disp, log)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewReceiver error %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestWebhookBodyLimit(t *testing.T) {
	event := githubtest.LabeledEvent().JSON()
	// Trailing whitespace keeps the JSON valid at any size.
	padded := func(size int) []byte {
		return append(bytes.Clone(event), bytes.Repeat([]byte(" "), size-len(event))...)
	}
	tests := []struct {
		name string
		size int
		want int
	}{
		{"at the limit", github.MaxBodyBytes, http.StatusAccepted},
		{"one byte over", github.MaxBodyBytes + 1, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, options{})
			wantStatus(t, h.send(t, "issues", padded(tt.size)), tt.want)
		})
	}
}

// stalledBody signals entered on its first Read and then blocks until
// release is closed, holding its delivery's in-flight slot.
type stalledBody struct {
	once    sync.Once
	entered chan<- struct{}
	release <-chan struct{}
}

func (b *stalledBody) Read([]byte) (int, error) {
	b.once.Do(func() { b.entered <- struct{}{} })
	<-b.release
	return 0, io.EOF
}

func TestWebhookConcurrencyLimit(t *testing.T) {
	h := newHarness(t, options{})
	entered := make(chan struct{}, github.MaxInFlight)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range github.MaxInFlight {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", &stalledBody{entered: entered, release: release})
		req.Header.Set("Content-Type", "application/json")
		wg.Go(func() { h.serve(req) })
	}
	for range github.MaxInFlight {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("stalled deliveries did not all start reading")
		}
	}

	wantStatus(t, h.send(t, "issues", githubtest.LabeledEvent().JSON()), http.StatusServiceUnavailable)

	close(release)
	wg.Wait()
	wantStatus(t, h.send(t, "issues", githubtest.LabeledEvent().JSON()), http.StatusAccepted)
}

func TestWebhookRejectsWrongMethodAndContentType(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		contentType string
		want        int
	}{
		{"GET", http.MethodGet, "application/json", http.StatusMethodNotAllowed},
		{"PUT", http.MethodPut, "application/json", http.StatusMethodNotAllowed},
		{"form encoded", http.MethodPost, "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"plain text", http.MethodPost, "text/plain", http.StatusUnsupportedMediaType},
		{"no content type", http.MethodPost, "", http.StatusUnsupportedMediaType},
		{"JSON with charset", http.MethodPost, "application/json; charset=utf-8", http.StatusAccepted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, options{})
			req := githubtest.Request(t, secret, "issues", githubtest.LabeledEvent().JSON())
			req.Method = tt.method
			req.Header.Set("Content-Type", tt.contentType)
			wantStatus(t, h.serve(req), tt.want)
		})
	}
}

func TestCommandFromNonOwnerIgnoredAndLogged(t *testing.T) {
	h := newHarness(t, options{})
	// A stranger who has taken the owner's old login: same login string,
	// different numeric ID.
	e := githubtest.CommentEvent("/revise canary_comment_text_never_logged")
	e.CommentUserID, e.SenderID = 666, 666
	e.CommentUserLogin, e.SenderLogin = githubtest.FixtureOwnerLogin, githubtest.FixtureOwnerLogin

	wantStatus(t, h.send(t, "issue_comment", e.JSON()), http.StatusAccepted)

	wantNoCalls(t, h)
	recs := h.records(t, "reason", "non_owner")
	if len(recs) != 1 {
		t.Fatalf("got %d non_owner log lines, want 1:\n%s", len(recs), h.logs.String())
	}
	if recs[0]["sender_id"] != float64(666) || recs[0]["level"] != "INFO" || recs[0]["delivery"] != githubtest.FixtureDeliveryID {
		t.Errorf("log line %v, want sender_id 666 at INFO with the delivery ID", recs[0])
	}
	if strings.Contains(h.logs.String(), "canary_comment_text") {
		t.Error("log carries the comment body")
	}
}

func appCommentCases() []struct {
	name       string
	mutate     func(e *githubtest.Event)
	wantReason string
} {
	return []struct {
		name       string
		mutate     func(e *githubtest.Event)
		wantReason string
	}{
		{"comment by this App", func(e *githubtest.Event) {
			e.CommentUserID, e.CommentUserType, e.CommentAppID = githubtest.FixtureAppBotID, "Bot", githubtest.FixtureAppID
			e.SenderID, e.SenderType = githubtest.FixtureAppBotID, "Bot"
		}, "platform"},
		{"comment by another bot", func(e *githubtest.Event) {
			e.CommentUserID, e.CommentUserType = 5555, "Bot"
			e.SenderID, e.SenderType = 5555, "Bot"
		}, "bot"},
		{"owner ID acting through another App", func(e *githubtest.Event) {
			e.CommentAppID = 88
		}, "bot"},
		{"stranger body forging the owner", func(e *githubtest.Event) {
			e.CommentUserID, e.SenderID = 666, 666
			e.CommentBody = "/revise posted by owner (id 1001)"
		}, "non_owner"},
		{"sender owner, comment user a stranger", func(e *githubtest.Event) {
			e.CommentUserID = 666
		}, "non_owner"},
		{"comment user owner, sender a stranger", func(e *githubtest.Event) {
			e.SenderID = 666
		}, "sender_mismatch"},
		{"comment user owner, sender a bot with the owner's ID", func(e *githubtest.Event) {
			e.SenderType = "Bot"
		}, "sender_mismatch"},
	}
}

func TestCommandInAppCommentTriggersNothing(t *testing.T) {
	for _, tt := range appCommentCases() {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, options{})
			e := githubtest.CommentEvent("/approve")
			tt.mutate(&e)

			wantStatus(t, h.send(t, "issue_comment", e.JSON()), http.StatusAccepted)

			wantNoCalls(t, h)
			if len(h.records(t, "reason", tt.wantReason)) != 1 {
				t.Errorf("want one log line with reason %q:\n%s", tt.wantReason, h.logs.String())
			}
		})
	}
}

func TestWebhookAcksBeforeDispatch(t *testing.T) {
	starter := &githubtest.FakeStarter{Block: make(chan struct{})}
	h := newHarness(t, options{starter: starter})

	began := time.Now()
	rec := h.send(t, "issues", githubtest.LabeledEvent().JSON())
	elapsed := time.Since(began)

	wantStatus(t, rec, http.StatusAccepted)
	if elapsed >= 2*time.Second {
		t.Fatalf("acknowledged after %s, want under 2s", elapsed)
	}
	if n := len(starter.Calls()); n != 0 {
		t.Fatalf("starter answered %d calls while blocked", n)
	}
	close(starter.Block)
	h.settle(t)
	if n := len(starter.Calls()); n != 1 {
		t.Fatalf("starter got %d calls after unblocking, want 1", n)
	}
}

func TestWebhookQueueFullReturns503(t *testing.T) {
	h := newHarness(t, options{depth: 2, noRun: true})
	body := githubtest.LabeledEvent().JSON()
	for range 2 {
		wantStatus(t, h.send(t, "issues", body), http.StatusAccepted)
	}

	wantStatus(t, h.sendAs(t, "0000-ffff", "issues", body), http.StatusServiceUnavailable)

	recs := h.records(t, "delivery", "0000-ffff")
	if len(recs) != 1 || recs[0]["level"] != "WARN" {
		t.Fatalf("want one WARN line for the refused delivery:\n%s", h.logs.String())
	}
}

func TestPingAccepted(t *testing.T) {
	h := newHarness(t, options{})
	wantStatus(t, h.send(t, "ping", []byte(`{"zen":"Design for failure.","hook_id":1}`)), http.StatusAccepted)
	wantNoCalls(t, h)
}
