package blob_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/blob"
)

// Namespaces for the planted links: each case gets its own, so one case's
// link cannot satisfy another's.
const (
	nsRelLink    = "11111111-1111-4111-8111-111111111111"
	nsAbsLink    = "22222222-2222-4222-8222-222222222222"
	nsFileLink   = "33333333-3333-4333-8333-333333333333"
	nsInRootLink = "44444444-4444-4444-8444-444444444444"
)

// sibling is a Dir at <tmp>/root beside <tmp>/root-evil, with links
// planted inside the root that point at the sibling.
type sibling struct {
	d          *blob.Dir
	root, evil string
	// hash names the blob planted in the sibling, under ns, and the same
	// content stored legitimately in the root under ns.
	hash string
}

func plantSibling(t *testing.T) sibling {
	t.Helper()
	tmp := t.TempDir()
	s := sibling{root: filepath.Join(tmp, "root"), evil: filepath.Join(tmp, "root-evil")}
	secret := []byte("canary_sibling_secret")
	s.hash = hashHex(secret)
	mkdirs(t, filepath.Join(s.root, nsFileLink), filepath.Join(s.evil, ns))
	if err := os.WriteFile(filepath.Join(s.evil, ns, s.hash), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	s.d = newDir(t, s.root)
	// A real namespace inside the root, for the in-root link to point at.
	if _, err := s.d.Put(context.Background(), ns, secret); err != nil {
		t.Fatal(err)
	}
	symlink(t, "../root-evil/"+ns, filepath.Join(s.root, nsRelLink))
	symlink(t, filepath.Join(s.evil, ns), filepath.Join(s.root, nsAbsLink))
	symlink(t, "../../root-evil/"+ns+"/"+s.hash, filepath.Join(s.root, nsFileLink, s.hash))
	symlink(t, ns, filepath.Join(s.root, nsInRootLink))
	return s
}

// TestAttackSiblingPrefix: a Dir rooted at <tmp>/root must neither read
// nor write <tmp>/root-evil, a sibling whose name shares the root's as a
// prefix, which is what a string-prefix containment check would let
// through. The sibling holds a blob that is valid in every respect but
// location, so any read that reaches it would pass the hash check. The
// os.Root handle alone refuses the links that leave the root; the link
// that stays inside it is refused by Dir's own symlink check.
func TestAttackSiblingPrefix(t *testing.T) {
	s := plantSibling(t)
	before := files(t, s.evil)
	h := s.hash

	gets := []struct{ name, key string }{
		{"dotdot", "../root-evil/" + ns + "/" + h},
		{"absolute", filepath.Join(s.evil, ns, h)},
		{"percent-encoded", "%2e%2e%2froot-evil%2f" + ns + "/" + h},
		{"sibling name as namespace", "root-evil/" + ns + "/" + h},
		{"relative symlinked namespace", nsRelLink + "/" + h},
		{"absolute symlinked namespace", nsAbsLink + "/" + h},
		{"symlinked blob", nsFileLink + "/" + h},
		{"namespace linked inside the root", nsInRootLink + "/" + h},
	}
	for _, tc := range gets {
		t.Run("get/"+tc.name, func(t *testing.T) { refusesGet(t, s.d, tc.key) })
	}

	puts := []struct{ name, ns string }{
		{"dotdot", "../root-evil/" + ns},
		{"dotdot alone", "../root-evil"},
		{"absolute", filepath.Join(s.evil, ns)},
		{"percent-encoded", "..%2froot-evil%2f" + ns},
		{"relative symlinked namespace", nsRelLink},
		{"absolute symlinked namespace", nsAbsLink},
		{"namespace linked inside the root", nsInRootLink},
	}
	for _, tc := range puts {
		t.Run("put/"+tc.name, func(t *testing.T) { refusesPut(t, s.d, tc.ns) })
	}

	if after := files(t, s.evil); !slices.Equal(after, before) {
		t.Fatalf("sibling changed: before %v, after %v", before, after)
	}
	if left := files(t, s.root); strings.Contains(strings.Join(left, " "), ".tmp-") {
		t.Fatalf("refused puts left temporary files: %v", left)
	}
}

func refusesGet(t *testing.T, d *blob.Dir, key string) {
	t.Helper()
	got, err := d.Get(context.Background(), key)
	if err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("got error %v, want a refusal", err)
	}
	if got != nil {
		t.Fatalf("returned %q", got)
	}
}

func refusesPut(t *testing.T, d *blob.Dir, ns string) {
	t.Helper()
	if key, err := d.Put(context.Background(), ns, []byte("canary_written_outside")); err == nil {
		t.Fatalf("stored as %s", key)
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
