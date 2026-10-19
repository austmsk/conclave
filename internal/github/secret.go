package github

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

const redacted = "[redacted]"

// ErrRefused reports a request this package will not send: a branch
// outside agent/, a body carrying a credential, a pull request it did not
// open. Retrying cannot change the answer.
var ErrRefused = errors.New("github: refused")

// secret holds a credential: an installation token, the App's JWT, the
// webhook secret. Every way of printing it prints [redacted], so a secret
// that ends up in a log line, a %+v, or a JSON document by mistake leaks
// nothing. reveal is the only way to the value, and only request
// construction calls it.
type secret struct{ v string }

func newSecret(v string) secret { return secret{v: v} }

func (s secret) reveal() string { return s.v }

func (s secret) empty() bool { return s.v == "" }

func (secret) String() string               { return redacted }
func (secret) GoString() string             { return redacted }
func (secret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// tokenPatterns match credential formats this package might see echoed
// back: GitHub installation, user, OAuth and refresh tokens, fine-grained
// personal access tokens, and JWTs such as the App's own. The prefixes are
// assembled from parts so the literal forms never appear in the source.
// They are not anchored at a word boundary: a token is still a token after
// a word character, as in a percent-encoded "token%3D" or a JSON-escaped
// "\n", and over-redacting a word is cheaper than leaking a credential.
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]` + `_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`github` + `_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
}

// redact replaces every known secret value and every token-shaped string
// in s with [redacted]. It is the last line of defence for text that
// passes through from git or GitHub; the first is never putting a secret
// in that text.
func redact(s string, known ...secret) string {
	for _, k := range known {
		if !k.empty() {
			s = strings.ReplaceAll(s, k.reveal(), redacted)
		}
	}
	for _, p := range tokenPatterns {
		s = p.ReplaceAllString(s, redacted)
	}
	return s
}

// refuseSecrets returns ErrRefused if body carries a known secret or a
// token-shaped string. Comment and pull request bodies pass through it
// before they are sent, so text that would publish a credential is never
// posted.
func refuseSecrets(body string, known ...secret) error {
	if redact(body, known...) != body {
		return fmt.Errorf("%w: body carries a credential", ErrRefused)
	}
	return nil
}

// RedactAttr is a slog.HandlerOptions.ReplaceAttr that removes
// token-shaped strings from every string attribute, the message included,
// and from error values. Processes install it on their root logger as a
// backstop; nothing in this package logs a secret to begin with.
func RedactAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(redact(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(redact(err.Error()))
		}
	}
	return a
}
