package github

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
)

// nonceBytes is the nonce's size before hex encoding: 128 bits, so a
// guessed closer never matches.
const nonceBytes = 16

// Quarantine wraps untrusted GitHub text for an agent's context (FR-SEC28).
// One Quarantine serves one model request: every block in that request
// shares its nonce, and no other request sees it.
//
//	<<untrusted nonce=N source=<source> author_id=<id>>>
//	text
//	<</untrusted nonce=N>>
//
// The nonce is stripped from the text before wrapping, so text cannot
// close its own block, and the attributes are an enum and an integer, never
// anything the author typed.
type Quarantine struct{ nonce string }

// NewQuarantine returns a Quarantine with a fresh 128-bit nonce, written
// as 32 lowercase hex characters.
func NewQuarantine() (*Quarantine, error) {
	b := make([]byte, nonceBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("reading quarantine nonce: %w", err)
	}
	return &Quarantine{nonce: hex.EncodeToString(b)}, nil
}

// Wrap returns text inside a block labelled with src and a's numeric ID.
// A src that is not one of the four sources is labelled a stranger.
// Invalid UTF-8 becomes U+FFFD, and every occurrence of the nonce, in any
// case, is removed, including one that a removal splices together. A
// Quarantine not made by NewQuarantine has no nonce and panics, rather
// than emit a block anyone could close.
func (q *Quarantine) Wrap(src Source, a Author, text string) string {
	if q.nonce == "" {
		panic("github: Quarantine used without a nonce; call NewQuarantine")
	}
	if !knownSource(src) {
		src = SourceStranger
	}
	text = stripFold(strings.ToValidUTF8(text, "\uFFFD"), q.nonce)

	var b strings.Builder
	b.Grow(len(text) + 2*len(q.nonce) + 80)
	b.WriteString("<<untrusted nonce=")
	b.WriteString(q.nonce)
	b.WriteString(" source=")
	b.WriteString(string(src))
	b.WriteString(" author_id=")
	b.WriteString(strconv.FormatInt(a.ID, 10))
	b.WriteString(">>\n")
	b.WriteString(text)
	b.WriteString("\n<</untrusted nonce=")
	b.WriteString(q.nonce)
	b.WriteString(">>")
	return b.String()
}

// Admit reports whether text from src may enter an agent's context on a
// repository of the given tier. Platform text never does: agents read
// canonical records, not the platform's own comments. Owner text always
// does. Bot and stranger text is excluded on public repositories and
// quarantined elsewhere. A source or tier this package does not know is
// treated as the least trusted it could be: an unknown source as a
// stranger, an unknown tier as public.
func Admit(src Source, tier contracts.SensitivityTier) bool {
	switch src {
	case SourceOwner:
		return true
	case SourcePlatform:
		return false
	}
	switch tier {
	case contracts.TierPrivate, contracts.TierSensitive, contracts.TierLocalOnly:
		return true
	default:
		return false
	}
}

func knownSource(src Source) bool {
	switch src {
	case SourceOwner, SourcePlatform, SourceBot, SourceStranger:
		return true
	default:
		return false
	}
}

// stripFold removes every ASCII-case-insensitive occurrence of nonce from
// text until none remains. It copies text into an output buffer one byte
// at a time and drops the tail whenever the tail spells the nonce, so an
// occurrence that a removal splices together is caught as it forms. The
// buffer never holds the nonce, which is the property Wrap needs, and the
// work is linear in len(text) times len(nonce): a search-and-cut loop
// would be quadratic on a body packed with nonces.
//
// The nonce is ASCII, so the removed bytes are whole characters and valid
// UTF-8 stays valid. No non-ASCII character folds to a hex digit, so an
// ASCII fold is the whole of case-insensitivity here.
func stripFold(text, nonce string) string {
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		out = append(out, text[i])
		if n := len(out) - len(nonce); n >= 0 && asciiEqualFold(out[n:], nonce) {
			out = out[:n]
		}
	}
	return string(out)
}

// asciiEqualFold reports whether b and s are equal when ASCII letters are
// compared without case. Other bytes compare exactly.
func asciiEqualFold(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		if lowerASCII(b[i]) != lowerASCII(s[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
