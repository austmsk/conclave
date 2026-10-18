package blob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
)

// ErrCorrupt reports a stored blob whose content no longer hashes to its
// key. Its bytes are never returned.
var ErrCorrupt = errors.New("blob content does not match its key")

// nsPattern is the namespace part of a key. Namespaces are repository IDs,
// which are UUIDs as Postgres prints them: lowercase, hyphenated. Nothing
// looser is accepted, so a namespace can never be a path: it has no
// separator, no dot, and no escape of any kind.
const nsPattern = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

var (
	nsRE  = regexp.MustCompile(`^` + nsPattern + `$`)
	keyRE = regexp.MustCompile(`^(` + nsPattern + `)/([0-9a-f]{64})$`)
)

// Dir is a Store on the local filesystem, confined to its directory by an
// os.Root handle. Each blob is a file <ns>/<sha256 hex> under the
// directory.
//
// Confinement has three layers. Namespaces and keys are matched against
// strict patterns before any filesystem call, so no input can name a path
// outside a namespace directory. The os.Root handle refuses any name that
// resolves outside the directory, through ".." or a symlink. And Dir
// itself refuses a symlink anywhere in a blob's path, so even a link that
// stays inside the directory cannot make one namespace's key read another
// namespace's file.
//
// A blob is written to a temporary file created exclusively in its
// namespace directory, synced, and renamed over its final name, so a
// reader never sees a partial blob and concurrent writers of the same
// content each succeed. A process that dies mid-write can leave a
// temporary file behind; no key can name one.
type Dir struct {
	root *os.Root
}

var _ Store = (*Dir)(nil)

// NewDir opens a Store rooted at dir, which must exist.
func NewDir(dir string) (*Dir, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening blob directory: %w", err)
	}
	return &Dir{root: root}, nil
}

// Put implements Store. Storing content that is already stored rewrites
// it under the same key, which also repairs a copy damaged on disk.
func (d *Dir) Put(ctx context.Context, ns string, data []byte) (string, error) {
	if !nsRE.MatchString(ns) {
		return "", fmt.Errorf("storing blob under namespace %q: %w", ns, ErrInvalidKey)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("storing blob under %s: %w", ns, err)
	}
	sum := sha256.Sum256(data)
	key := ns + "/" + hex.EncodeToString(sum[:])
	if err := d.makeNamespace(ns); err != nil {
		return "", fmt.Errorf("storing blob %s: %w", key, err)
	}
	if err := d.write(ns, key, data); err != nil {
		return "", fmt.Errorf("storing blob %s: %w", key, err)
	}
	return key, nil
}

// Get implements Store.
func (d *Dir) Get(ctx context.Context, key string) ([]byte, error) {
	m := keyRE.FindStringSubmatch(key)
	if m == nil {
		return nil, fmt.Errorf("reading blob %q: %w", key, ErrInvalidKey)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", key, err)
	}
	data, err := d.read(m[1], key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("reading blob %s: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", key, err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != m[2] {
		return nil, fmt.Errorf("reading blob %s: %w", key, ErrCorrupt)
	}
	return data, nil
}

// Close releases the root handle.
func (d *Dir) Close() error {
	if err := d.root.Close(); err != nil {
		return fmt.Errorf("closing blob directory: %w", err)
	}
	return nil
}

func (d *Dir) makeNamespace(ns string) error {
	if err := d.root.Mkdir(ns, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("creating namespace directory: %w", err)
	}
	return d.checkNamespace(ns)
}

// checkNamespace refuses a namespace that is anything but a real
// directory: in particular a symlink, even one that stays in the root.
func (d *Dir) checkNamespace(ns string) error {
	fi, err := d.root.Lstat(ns)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("namespace %s is a %s, not a directory", ns, fi.Mode().Type())
	}
	return nil
}

func (d *Dir) write(ns, key string, data []byte) error {
	tmp := key + ".tmp-" + rand.Text()
	f, err := d.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = d.root.Remove(tmp)
		}
	}()
	if err := writeSynced(f, data); err != nil {
		return err
	}
	if err := d.root.Rename(tmp, key); err != nil {
		return fmt.Errorf("renaming into place: %w", err)
	}
	renamed = true
	return d.syncDir(ns)
}

// writeSynced writes data to f, syncs it, and closes it, so the rename
// that follows never publishes a blob whose content is not yet on disk.
func writeSynced(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing temporary file: %w", err)
	}
	return nil
}

// syncDir makes the rename itself durable.
func (d *Dir) syncDir(ns string) error {
	dir, err := d.root.Open(ns)
	if err != nil {
		return fmt.Errorf("opening namespace directory to sync: %w", err)
	}
	err = dir.Sync()
	if cerr := dir.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("syncing namespace directory: %w", err)
	}
	return nil
}

// read returns the content of the regular file at key. The file is
// checked with Lstat and then opened, and the open file must be the one
// checked, so a symlink swapped in between is refused rather than
// followed.
func (d *Dir) read(ns, key string) ([]byte, error) {
	if err := d.checkNamespace(ns); err != nil {
		return nil, err
	}
	want, err := d.root.Lstat(key)
	if err != nil {
		return nil, err
	}
	if !want.Mode().IsRegular() {
		return nil, fmt.Errorf("blob is a %s, not a regular file", want.Mode().Type())
	}
	f, err := d.root.Open(key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	got, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(want, got) {
		return nil, errors.New("blob was replaced while it was being opened")
	}
	return io.ReadAll(f)
}
