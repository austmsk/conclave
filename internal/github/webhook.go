package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"
)

const (
	// MaxBodyBytes caps a delivery's body. An issue and a comment of
	// GitHub's 65,536-character maximum, JSON-escaped, fit well inside it.
	MaxBodyBytes = 1 << 20
	// MaxInFlight caps deliveries being read at once, bounding memory
	// under a flood.
	MaxInFlight = 32

	signatureHeader = "X-Hub-Signature-256"
	signaturePrefix = "sha256="
	// badSignature is the whole response to a delivery that fails
	// verification. It is fixed, so the response says nothing about which
	// check failed or what the expected value was.
	badSignature = "invalid signature"
)

// deliveryShape is what a GitHub delivery GUID looks like. The header is
// the sender's text, and is logged only when it fits.
var deliveryShape = regexp.MustCompile(`^[0-9a-fA-F-]{1,64}$`)

// Receiver is the webhook endpoint. It verifies the delivery's signature
// before reading anything else, decides from the payload's author fields
// whether the event is the owner's, and enqueues accepted work on its
// Dispatcher. It never waits on the starter, so GitHub is answered in
// microseconds (NFR-P1).
type Receiver struct {
	cfg     Config
	enabled map[string]bool
	secret  secret
	disp    *Dispatcher
	log     *slog.Logger
	// inflight is a semaphore of MaxInFlight slots. A delivery that finds
	// none free is refused rather than queued, so a flood of slow bodies
	// cannot hold unbounded memory or goroutines.
	inflight chan struct{}
}

// NewReceiver returns a receiver for the repositories in enabled, keyed by
// full name. The secret must be at least MinWebhookSecretLen bytes: a short
// one could be guessed, and every delivery would then be forgeable
// (FR-SEC2). Full names are compared case-insensitively, as GitHub does.
func NewReceiver(cfg Config, enabled map[string]bool, webhookSecret []byte, d *Dispatcher, log *slog.Logger) (*Receiver, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("webhook receiver: %w", err)
	}
	if len(webhookSecret) < MinWebhookSecretLen {
		return nil, fmt.Errorf("webhook receiver: %w: webhook secret is shorter than %d bytes",
			ErrInvalidConfig, MinWebhookSecretLen)
	}
	if d == nil || log == nil {
		return nil, errors.New("webhook receiver: dispatcher and logger are required")
	}
	lower := make(map[string]bool, len(enabled))
	for name, on := range enabled {
		if on {
			lower[strings.ToLower(name)] = true
		}
	}
	return &Receiver{
		cfg:      cfg,
		enabled:  lower,
		secret:   newSecret(string(webhookSecret)),
		disp:     d,
		log:      log,
		inflight: make(chan struct{}, MaxInFlight),
	}, nil
}

// ServeHTTP handles one delivery. Nothing from the delivery is parsed, and
// no header but the signature is read, until the signature has been
// verified against the raw body; a forged delivery costs one HMAC and
// leaves one line in no log.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if status := checkShape(req); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	select {
	case r.inflight <- struct{}{}:
		defer func() { <-r.inflight }()
	default:
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	body, status := readBody(w, req)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	if !r.verified(req.Header.Values(signatureHeader), body) {
		http.Error(w, badSignature, http.StatusUnauthorized)
		return
	}
	w.WriteHeader(r.handle(req.Context(), req.Header, body))
}

// checkShape refuses anything but a JSON POST, before a slot or a byte of
// the body is spent on it.
func checkShape(req *http.Request) int {
	if req.Method != http.MethodPost {
		return http.StatusMethodNotAllowed
	}
	mt, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return http.StatusUnsupportedMediaType
	}
	return 0
}

// readBody reads at most MaxBodyBytes. It returns a status when the body
// is too large or cannot be read.
func readBody(w http.ResponseWriter, req *http.Request) ([]byte, int) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, MaxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, http.StatusRequestEntityTooLarge
	case err != nil:
		return nil, http.StatusBadRequest
	}
	return body, 0
}

// verified reports whether the delivery carries exactly one SHA-256
// signature of body under the webhook secret. The comparison is constant
// time (hmac.Equal). The SHA-1 header GitHub also sends is never read.
func (r *Receiver) verified(headers []string, body []byte) bool {
	if len(headers) != 1 {
		return false
	}
	digest, ok := strings.CutPrefix(headers[0], signaturePrefix)
	if !ok || len(digest) != 2*sha256.Size {
		return false
	}
	got, err := hex.DecodeString(digest)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(r.secret.reveal()))
	_, _ = mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// handle classifies a verified delivery and enqueues any work, returning
// the status to answer with. GitHub is answered 202 for everything it need
// not redeliver, ignored events included; 503 asks for a redelivery.
func (r *Receiver) handle(ctx context.Context, h http.Header, body []byte) int {
	delivery := deliveryID(h.Get("X-GitHub-Delivery"))
	event := h.Get("X-GitHub-Event")
	j, skip, err := classify(r.cfg, r.enabled, event, delivery, body)
	if err != nil {
		// The decode error is not logged: its text can quote the body.
		r.log.Info("webhook delivery undecodable", "delivery", delivery, "event", eventName(event))
		return http.StatusBadRequest
	}
	if skip.reason != "" {
		r.logIgnored(ctx, skip, delivery, event)
		return http.StatusAccepted
	}
	if !r.disp.enqueue(j) {
		r.log.Warn("webhook delivery refused: dispatch queue full or closed", "delivery", delivery)
		return http.StatusServiceUnavailable
	}
	return http.StatusAccepted
}

// attentionReasons are ignored deliveries the owner should see: someone
// other than the owner, or a command the owner mistyped. Everything else is
// routine and logged at debug.
var attentionReasons = map[string]bool{
	reasonNonOwner:         true,
	reasonBot:              true,
	reasonPlatform:         true,
	reasonSenderMismatch:   true,
	reasonMalformedCommand: true,
}

// logIgnored records why a delivery did nothing, with the sender's numeric
// ID and never the payload's text.
func (r *Receiver) logIgnored(ctx context.Context, skip ignore, delivery, event string) {
	level := slog.LevelDebug
	if attentionReasons[skip.reason] {
		level = slog.LevelInfo
	}
	r.log.Log(ctx, level, "webhook delivery ignored",
		"reason", skip.reason,
		"sender_id", skip.senderID,
		"delivery", delivery,
		"event", eventName(event),
	)
}

// deliveryID returns the delivery header if it is GUID-shaped, else
// "invalid", so a header cannot write arbitrary text into the log.
func deliveryID(h string) string {
	if deliveryShape.MatchString(h) {
		return h
	}
	return "invalid"
}
