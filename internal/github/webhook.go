package github

import (
	"log/slog"
	"net/http"
)

const (
	// MaxBodyBytes caps a delivery's body. An issue and a comment of
	// GitHub's 65,536-character maximum, JSON-escaped, fit well inside it.
	MaxBodyBytes = 1 << 20
	// MaxInFlight caps deliveries being read at once, bounding memory
	// under a flood.
	MaxInFlight = 32
)

// Receiver is the webhook endpoint. It verifies the delivery's signature
// before reading anything else, decides from the payload's author fields
// whether the event is the owner's, and enqueues accepted work on its
// Dispatcher. It never waits on the starter, so GitHub is answered in
// microseconds (NFR-P1).
//
// L0 stub: W1 implements.
type Receiver struct {
	cfg     Config
	enabled map[string]bool
	secret  secret
	disp    *Dispatcher
	log     *slog.Logger
}

// NewReceiver returns a receiver for the repositories in enabled, keyed by
// full name. The secret must be at least MinWebhookSecretLen bytes.
//
// L0 stub: W1 implements.
func NewReceiver(cfg Config, enabled map[string]bool, webhookSecret []byte, d *Dispatcher, log *slog.Logger) (*Receiver, error) {
	return &Receiver{cfg: cfg, enabled: enabled, secret: newSecret(string(webhookSecret)), disp: d, log: log}, nil
}

// ServeHTTP handles one delivery.
//
// L0 stub: W1 implements.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
}
