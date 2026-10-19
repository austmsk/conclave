package githubtest

import (
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
	"github.com/austmsk/conclave/internal/testenv"
)

// GitServer serves the bare repositories under a root over smart HTTP
// through git's own CGI backend, fetches and pushes both, and rejects every
// request that does not carry the installation token as GitHub expects
// it: Basic auth for the x-access-token user. It counts the requests it
// authorized and rejected.
type GitServer struct {
	*httptest.Server
	authorized atomic.Int64
	rejected   atomic.Int64
}

// NewGitServer serves root, accepting only token. It skips the test, or
// fails it under testenv.RequireEnv, when git or git-http-backend is
// missing.
func NewGitServer(t *testing.T, root, token string) *GitServer {
	t.Helper()
	backend := httpBackend(t)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	handler := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			// git-http-backend serves receive-pack only to an
			// authenticated user; the handler below is the
			// authentication.
			"REMOTE_USER=x-access-token",
		},
	}
	s := &GitServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			s.rejected.Add(1)
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.authorized.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// Authorized is the number of requests that carried the token.
func (s *GitServer) Authorized() int64 { return s.authorized.Load() }

// Rejected is the number of requests refused for want of the token.
func (s *GitServer) Rejected() int64 { return s.rejected.Load() }

// Requests is the number of requests received.
func (s *GitServer) Requests() int64 { return s.Authorized() + s.Rejected() }

func httpBackend(t *testing.T) string {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		testenv.Skip(t, "git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		testenv.Skip(t, "git-http-backend not available: %v", err)
	}
	return backend
}

// BareDir is where fullName's bare repository lives under root.
func BareDir(root, fullName string) string {
	return filepath.Join(root, filepath.FromSlash(fullName)+".git")
}

// NewBareRepo creates fullName's bare repository under root with one
// commit on main, and returns that commit.
func NewBareRepo(t *testing.T, root, fullName string) string {
	t.Helper()
	work := t.TempDir()
	sandboxtest.Git(t, work, "init", "-q", "-b", "main", ".")
	sandboxtest.WriteFile(t, work, "README.md", "# Election Tally\n")
	sandboxtest.Git(t, work, "add", "-A")
	sandboxtest.Git(t, work, "commit", "-q", "-m", "initial")
	head := strings.TrimSpace(sandboxtest.Git(t, work, "rev-parse", "HEAD"))
	sandboxtest.Git(t, root, "clone", "--quiet", "--bare", work, BareDir(root, fullName))
	return head
}

// RefValue returns the commit ref points at in fullName's bare repository,
// or "" when ref does not exist.
func RefValue(t *testing.T, root, fullName, ref string) string {
	t.Helper()
	return strings.TrimSpace(sandboxtest.Git(t, BareDir(root, fullName), "for-each-ref", "--format=%(objectname)", ref))
}

// RejectPushes installs a pre-receive hook in fullName's bare repository
// that prints message and refuses every push, as GitHub refuses an App's
// push that edits a workflow file without the workflows permission.
func RejectPushes(t *testing.T, root, fullName, message string) {
	t.Helper()
	hooks := filepath.Join(BareDir(root, fullName), "hooks")
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive-message"), []byte(message+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncat >/dev/null\ncat \"$(dirname \"$0\")/pre-receive-message\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// RedirectServer answers every request with a 307 to the same path on
// another server, and counts the requests it received.
type RedirectServer struct {
	*httptest.Server
	requests atomic.Int64
}

// NewRedirectServer redirects every request to target.
func NewRedirectServer(t *testing.T, target string) *RedirectServer {
	t.Helper()
	s := &RedirectServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(s.Close)
	return s
}

// Requests is the number of requests received.
func (s *RedirectServer) Requests() int64 { return s.requests.Load() }
