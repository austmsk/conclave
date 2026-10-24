package gate

import "context"

// Finding is one secret-scanner match. It never holds the matched text.
type Finding struct {
	RuleID string
	Path   string
	Line   int
}

// SecretScanner scans text for secrets. It is an external-process
// boundary: the production implementation runs a pinned gitleaks binary.
// An error is an environment failure, never a verdict.
type SecretScanner interface {
	Scan(ctx context.Context, input []byte) ([]Finding, error)
}
