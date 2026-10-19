package github

import (
	"context"
	"time"
)

// Test hooks. They reach unexported state that tests must control and
// production code must not: the clock, the token source, the nonce.

// SetClock replaces the App's clock.
func SetClock(a *App, now func() time.Time) { a.now = now }

// StubToken makes the App hand out token for every repository without
// asking GitHub, for tests of what the App does with a token.
func StubToken(a *App, token string) {
	a.token = func(context.Context, RepoRef) (secret, error) { return newSecret(token), nil }
}

// QuarantineWithNonce returns a Quarantine with a known nonce.
func QuarantineWithNonce(nonce string) *Quarantine { return &Quarantine{nonce: nonce} }

// Redact exposes redact with known values given as strings.
func Redact(s string, known ...string) string { return redact(s, secrets(known)...) }

// RefuseSecrets exposes refuseSecrets with known values given as strings.
func RefuseSecrets(body string, known ...string) error { return refuseSecrets(body, secrets(known)...) }

// NewSecret returns v held as a secret, for formatting tests.
func NewSecret(v string) any { return newSecret(v) }

func secrets(vs []string) []secret {
	out := make([]secret, len(vs))
	for i, v := range vs {
		out[i] = newSecret(v)
	}
	return out
}
