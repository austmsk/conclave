package sandboxtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CanaryToken is the fake installation token planted in every fixture's
// remote URL. It follows the canary format so a leak is unmistakable.
const CanaryToken = "canary_installation_token_do_not_leak"

// RemoteURL is the authenticated clone URL the fixture records, in the
// shape a clone over a credentialed URL would leave behind.
const RemoteURL = "https://x-access-token:" + CanaryToken + "@github.com/austmsk/election-tally.git"

// NewRepo creates a repository under t.TempDir() with one commit, a
// submodule at lib/, an authenticated remote, a credential helper, a
// reflog mentioning the remote URL, and a packed remote-tracking ref. The
// submodule's git directory under .git/modules carries the same token in
// its config and reflog. It returns the tree path and the HEAD commit.
func NewRepo(t *testing.T) (string, string) {
	t.Helper()
	sub := newSubmoduleSource(t)
	dir := t.TempDir()
	Git(t, dir, "init", "-q", "-b", "main", ".")
	Git(t, dir, "config", "user.name", "Fixture")
	Git(t, dir, "config", "user.email", "fixture@example.invalid")
	WriteFile(t, dir, "README.md", "# Election Tally\n")
	WriteFile(t, dir, "src/tally.go", "package tally\n\nfunc Count() int { return 0 }\n")
	WriteFile(t, dir, ".gitignore", "node_modules/\n")
	Git(t, dir, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", sub, "lib")
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-q", "-m", "initial")
	head := strings.TrimSpace(Git(t, dir, "rev-parse", "HEAD"))

	Git(t, dir, "remote", "add", "origin", RemoteURL)
	Git(t, dir, "config", "credential.helper", "store")
	Git(t, dir, "config", "http.https://github.com/.extraheader", "Authorization: Basic "+CanaryToken)
	Git(t, dir, "update-ref", "refs/remotes/origin/main", head)
	Git(t, dir, "pack-refs", "--all")
	// Reflog lines of the kind a clone writes, in both git directories.
	WriteFile(t, dir, ".git/logs/HEAD", head+" "+head+" Fixture <f@x> 0 +0000\tclone: from "+RemoteURL+"\n")
	WriteFile(t, dir, ".git/modules/lib/logs/HEAD", head+" "+head+" Fixture <f@x> 0 +0000\tclone: from "+RemoteURL+"\n")
	// A submodule cloned through a credentialed superproject URL records
	// the same credentials in its own remote.
	Git(t, filepath.Join(dir, "lib"), "remote", "set-url", "origin", RemoteURL+"/lib")
	return dir, head
}

// newSubmoduleSource builds the repository the fixture's submodule points
// at.
func newSubmoduleSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	Git(t, dir, "init", "-q", "-b", "main", ".")
	Git(t, dir, "config", "user.name", "Fixture")
	Git(t, dir, "config", "user.email", "fixture@example.invalid")
	WriteFile(t, dir, "lib.go", "package lib\n")
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-q", "-m", "lib")
	return dir
}

// NewBareRemotes builds a bare superproject and a bare submodule source
// under one root, laid out for serving over HTTP: super.git's .gitmodules
// points at ../sub.git, so a clone resolves the submodule against the
// same server. It returns the root and the superproject's HEAD.
func NewBareRemotes(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	sub := newSubmoduleSource(t)
	Git(t, root, "clone", "--quiet", "--bare", sub, "sub.git")

	work := t.TempDir()
	Git(t, work, "init", "-q", "-b", "main", ".")
	Git(t, work, "config", "user.name", "Fixture")
	Git(t, work, "config", "user.email", "fixture@example.invalid")
	WriteFile(t, work, "README.md", "# Election Tally\n")
	Git(t, work, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", sub, "lib")
	Git(t, work, "config", "-f", ".gitmodules", "submodule.lib.url", "../sub.git")
	Git(t, work, "add", "-A")
	Git(t, work, "commit", "-q", "-m", "initial")
	head := strings.TrimSpace(Git(t, work, "rev-parse", "HEAD"))
	Git(t, root, "clone", "--quiet", "--bare", work, "super.git")
	return root, head
}

// Git runs git in dir with a scrubbed environment and fails the test on
// error. It returns stdout.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"LANG=C",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// WriteFile writes rel under dir, creating parents.
func WriteFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ContainsToken reports every file under root whose contents mention the
// canary token.
func ContainsToken(t *testing.T, root string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), CanaryToken) {
			rel, _ := filepath.Rel(root, path)
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}
