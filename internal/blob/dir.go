package blob

import (
	"context"
	"errors"
)

// Dir is a Store on the local filesystem, confined to its directory by an
// os.Root handle.
type Dir struct{}

var _ Store = (*Dir)(nil)

// NewDir opens a Store rooted at dir, which must exist.
func NewDir(dir string) (*Dir, error) {
	return nil, errors.New("blob.NewDir: not implemented")
}

// Put implements Store.
func (d *Dir) Put(ctx context.Context, ns string, data []byte) (string, error) {
	return "", errors.New("blob.Dir.Put: not implemented")
}

// Get implements Store.
func (d *Dir) Get(ctx context.Context, key string) ([]byte, error) {
	return nil, errors.New("blob.Dir.Get: not implemented")
}

// Close releases the root handle.
func (d *Dir) Close() error { return nil }
