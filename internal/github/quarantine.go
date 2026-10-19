package github

import "github.com/austmsk/conclave/internal/contracts"

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

// NewQuarantine returns a Quarantine with a fresh 128-bit nonce.
//
// L0 stub: W4 implements.
func NewQuarantine() (*Quarantine, error) {
	return &Quarantine{}, nil
}

// Wrap returns text inside a block labelled with src and a's numeric ID.
//
// L0 stub: W4 implements.
func (q *Quarantine) Wrap(src Source, a Author, text string) string {
	_, _ = src, a
	return text
}

// Admit reports whether text from src may enter an agent's context on a
// repository of the given tier. Platform text never does: agents read
// canonical records, not the platform's own comments. Owner text always
// does. Bot and stranger text is excluded on public repositories and
// quarantined elsewhere.
//
// L0 stub: W4 implements.
func Admit(src Source, tier contracts.SensitivityTier) bool {
	_, _ = src, tier
	return false
}
