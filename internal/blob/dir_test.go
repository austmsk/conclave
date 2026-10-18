package blob_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/austmsk/conclave/internal/blob"
)

const ns = "0f6b1c2a-5d1e-4c8e-9a4b-1f2e3d4c5b6a"

func newDir(t *testing.T, dir string) *blob.Dir {
	t.Helper()
	d, err := blob.NewDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// files lists every entry under dir, relative to it, so a test can show
// that nothing was written where it should not be.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		names = append(names, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return names
}

func TestPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := newDir(t, t.TempDir())
	data := []byte("full output of run_tests\n")

	key, err := d.Put(ctx, ns, data)
	if err != nil {
		t.Fatal(err)
	}
	if want := ns + "/" + hashHex(data); key != want {
		t.Fatalf("key = %s, want %s", key, want)
	}
	got, err := d.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("Get = %q, want %q", got, data)
	}
}

// TestPutIsIdempotent: storing the same output twice yields the same key
// and one file, with no temporary file left behind.
func TestPutIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := newDir(t, dir)
	data := []byte("same output")

	first, err := d.Put(ctx, ns, data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.Put(ctx, ns, data)
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if first != second {
		t.Fatalf("keys differ: %s, %s", first, second)
	}
	want := []string{".", ns, filepath.FromSlash(first)}
	if got := files(t, dir); !slices.Equal(got, want) {
		t.Fatalf("directory holds %v, want %v", got, want)
	}
}

// TestPutConcurrent: writers racing to store the same content all succeed
// with the same key, and the blob reads back whole.
func TestPutConcurrent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := newDir(t, dir)
	data := []byte(strings.Repeat("concurrent output\n", 4096))

	const writers = 16
	keys := make([]string, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() { keys[i], errs[i] = d.Put(ctx, ns, data) })
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
		if keys[i] != keys[0] {
			t.Fatalf("writer %d got key %s, writer 0 got %s", i, keys[i], keys[0])
		}
	}
	got, err := d.Get(ctx, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatal("blob read back differs from what was stored")
	}
	if n := len(files(t, dir)); n != 3 {
		t.Fatalf("directory holds %d entries, want 3 (root, namespace, blob): %v", n, files(t, dir))
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	ctx := context.Background()
	d := newDir(t, t.TempDir())
	key := ns + "/" + hashHex([]byte("never stored"))
	if _, err := d.Get(ctx, key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("missing namespace: got %v, want ErrNotFound", err)
	}
	if _, err := d.Put(ctx, ns, []byte("something else")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Get(ctx, key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("missing blob: got %v, want ErrNotFound", err)
	}
}

// TestGetVerifiesHash tampers with stored blobs on disk. Get must refuse
// each one and return none of its bytes.
func TestGetVerifiesHash(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := newDir(t, dir)
	other, err := d.Put(ctx, ns, []byte("another blob"))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		tamper func(path string) error
	}{
		{"content replaced", func(p string) error { return os.WriteFile(p, []byte("canary_tampered"), 0o600) }},
		{"content truncated", func(p string) error { return os.Truncate(p, 3) }},
		{"content appended", func(p string) error {
			f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			_, err = f.WriteString("canary_appended")
			return errors.Join(err, f.Close())
		}},
		{"another blob's content", func(p string) error {
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(other)))
			if err != nil {
				return err
			}
			return os.WriteFile(p, b, 0o600)
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := d.Put(ctx, ns, []byte("original output "+tc.name))
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.tamper(filepath.Join(dir, filepath.FromSlash(key))); err != nil {
				t.Fatalf("tampering (case %d): %v", i, err)
			}
			got, err := d.Get(ctx, key)
			if !errors.Is(err, blob.ErrCorrupt) {
				t.Fatalf("got %v, want ErrCorrupt", err)
			}
			if got != nil {
				t.Fatalf("returned %q alongside the error", got)
			}
		})
	}
}

// TestKeyFormat: anything but <uuid>/<64 lowercase hex> is refused before
// storage is touched.
func TestKeyFormat(t *testing.T) {
	ctx := context.Background()
	d := newDir(t, t.TempDir())
	h := hashHex([]byte("x"))

	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"no namespace", h},
		{"no hash", ns + "/"},
		{"namespace only", ns},
		{"short hash", ns + "/" + h[:63]},
		{"long hash", ns + "/" + h + "0"},
		{"uppercase hash", ns + "/" + strings.ToUpper(h)},
		{"uppercase namespace", strings.ToUpper(ns) + "/" + h},
		{"unhyphenated namespace", strings.ReplaceAll(ns, "-", "") + "/" + h},
		{"namespace not a UUID", "repo-1/" + h},
		{"extra component", ns + "/" + ns + "/" + h},
		{"leading slash", "/" + ns + "/" + h},
		{"trailing slash", ns + "/" + h + "/"},
		{"trailing newline", ns + "/" + h + "\n"},
		{"dot component", ns + "/./" + h},
		{"dotdot", "../" + ns + "/" + h},
		{"backslash", ns + `\` + h},
		{"percent-encoded slash", ns + "%2f" + h},
		{"NUL", ns + "/" + h + "\x00"},
		{"temporary file", ns + "/" + h + ".tmp-ABC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := d.Get(ctx, tc.key); !errors.Is(err, blob.ErrInvalidKey) {
				t.Fatalf("got %v, want ErrInvalidKey", err)
			}
		})
	}
}

// TestNamespaceFormat: Put checks its namespace against the same pattern
// Get uses, and writes nothing for one that fails.
func TestNamespaceFormat(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := newDir(t, dir)

	cases := []struct {
		name string
		ns   string
	}{
		{"empty", ""},
		{"dot", "."},
		{"dotdot", ".."},
		{"uppercase", strings.ToUpper(ns)},
		{"not a UUID", "repo-1"},
		{"two components", ns + "/" + ns},
		{"trailing slash", ns + "/"},
		{"trailing newline", ns + "\n"},
		{"braced", "{" + ns + "}"},
		{"absolute", "/" + ns},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := d.Put(ctx, tc.ns, []byte("data")); !errors.Is(err, blob.ErrInvalidKey) {
				t.Fatalf("got %v, want ErrInvalidKey", err)
			}
		})
	}
	if got := files(t, dir); !slices.Equal(got, []string{"."}) {
		t.Fatalf("refused puts wrote %v", got)
	}
}

func TestContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := newDir(t, t.TempDir())
	if _, err := d.Put(ctx, ns, []byte("data")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put: got %v, want context.Canceled", err)
	}
	if _, err := d.Get(ctx, ns+"/"+hashHex([]byte("data"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get: got %v, want context.Canceled", err)
	}
}
