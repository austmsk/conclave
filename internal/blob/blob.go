package blob

import (
	"context"
	"errors"
)

var (
	// ErrNotFound reports a well-formed key with no stored blob.
	ErrNotFound = errors.New("blob not found")
	// ErrInvalidKey reports a key or namespace that does not match the
	// key format. It is refused before touching storage.
	ErrInvalidKey = errors.New("invalid blob key")
)

// Store is content-addressed blob storage. Implementations must be safe
// for concurrent use, and Put must be idempotent: the same data under the
// same namespace always yields the same key.
type Store interface {
	// Put stores data under ns and returns its key, <ns>/<sha256 hex>.
	Put(ctx context.Context, ns string, data []byte) (string, error)

	// Get returns the blob stored under key, verifying that its content
	// still hashes to the key.
	Get(ctx context.Context, key string) ([]byte, error)
}
