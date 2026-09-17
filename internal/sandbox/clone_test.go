package sandbox_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

// gitServer serves bare repositories over smart HTTP through git's own
// CGI backend, and rejects every request that does not carry the expected
// Authorization header. It records the number of authorized requests.
type gitServer struct {
	*httptest.Server
	authorized atomic.Int64
	rejected   atomic.Int64
}

func newGitServer(t *testing.T, root, token string) *gitServer {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	handler := &cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	s := &gitServer{}
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

func TestCloneNeverWritesTheTokenAnywhere(t *testing.T) {
	root, head := sandboxtest.NewBareRemotes(t)
	srv := newGitServer(t, root, sandboxtest.CanaryToken)
	dir := filepath.Join(t.TempDir(), "clone")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := sandbox.Clone(ctx, sandbox.CloneSpec{
		URL:        srv.URL + "/super.git",
		Dir:        dir,
		Ref:        "main",
		Token:      sandboxtest.CanaryToken,
		Submodules: true,
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if srv.authorized.Load() == 0 || srv.rejected.Load() != 0 {
		t.Fatalf("server saw %d authorized and %d rejected requests; the token must arrive as a header on every request", srv.authorized.Load(), srv.rejected.Load())
	}

	if hits := sandboxtest.ContainsToken(t, dir); len(hits) != 0 {
		t.Errorf("token written to %v", hits)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "modules", "lib", "config")); err != nil {
		t.Fatalf("submodule was not cloned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lib", "lib.go")); err != nil {
		t.Errorf("submodule tree missing: %v", err)
	}
	if err := sandbox.VerifyTree(dir); err != nil {
		t.Errorf("clone is not a prepared tree: %v", err)
	}
	if got, err := sandbox.HeadCommit(dir); err != nil || got != head {
		t.Errorf("HeadCommit = %q, %v; want %q", got, err, head)
	}
	if out := sandboxtest.Git(t, dir, "remote"); strings.TrimSpace(out) != "" {
		t.Errorf("remotes remain after clone: %q", out)
	}
}

func TestCloneAtACommit(t *testing.T) {
	root, head := sandboxtest.NewBareRemotes(t)
	srv := newGitServer(t, root, sandboxtest.CanaryToken)
	dir := filepath.Join(t.TempDir(), "clone")

	err := sandbox.Clone(context.Background(), sandbox.CloneSpec{
		URL: srv.URL + "/super.git", Dir: dir, Ref: head, Token: sandboxtest.CanaryToken, Submodules: true,
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if got, err := sandbox.HeadCommit(dir); err != nil || got != head {
		t.Errorf("HeadCommit = %q, %v; want %q", got, err, head)
	}
	if hits := sandboxtest.ContainsToken(t, dir); len(hits) != 0 {
		t.Errorf("token written to %v", hits)
	}
	if srv.rejected.Load() != 0 {
		t.Errorf("%d requests went out without the token", srv.rejected.Load())
	}
}

func TestCloneWithWrongTokenFailsAndLeavesNoTree(t *testing.T) {
	root, _ := sandboxtest.NewBareRemotes(t)
	srv := newGitServer(t, root, sandboxtest.CanaryToken)
	dir := filepath.Join(t.TempDir(), "clone")

	err := sandbox.Clone(context.Background(), sandbox.CloneSpec{
		URL: srv.URL + "/super.git", Dir: dir, Token: "canary_wrong_token",
	})
	if err == nil {
		t.Fatal("clone with a rejected token should fail")
	}
	if strings.Contains(err.Error(), "canary_wrong_token") {
		t.Errorf("error message carries the token: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil {
		t.Error("a failed clone left a git directory behind")
	}
}

func TestCloneRejectsUnsafeSpecs(t *testing.T) {
	cases := []struct {
		name string
		spec sandbox.CloneSpec
	}{
		{"credentials in URL", sandbox.CloneSpec{URL: sandboxtest.RemoteURL, Dir: filepath.Join(t.TempDir(), "x")}},
		{"plain http to a remote host", sandbox.CloneSpec{URL: "http://github.com/a/b.git", Dir: filepath.Join(t.TempDir(), "x")}},
		{"ssh", sandbox.CloneSpec{URL: "ssh://git@github.com/a/b.git", Dir: filepath.Join(t.TempDir(), "x")}},
		{"file", sandbox.CloneSpec{URL: "file:///etc", Dir: filepath.Join(t.TempDir(), "x")}},
		{"existing directory", sandbox.CloneSpec{URL: "https://github.com/a/b.git", Dir: t.TempDir()}},
		{"empty directory", sandbox.CloneSpec{URL: "https://github.com/a/b.git"}},
		{"ref that is an option", sandbox.CloneSpec{URL: "https://github.com/a/b.git", Dir: filepath.Join(t.TempDir(), "x"), Ref: "--upload-pack=evil"}},
		{"negative depth", sandbox.CloneSpec{URL: "https://github.com/a/b.git", Dir: filepath.Join(t.TempDir(), "x"), Depth: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := sandbox.Clone(context.Background(), tc.spec); !errors.Is(err, sandbox.ErrCloneSpec) {
				t.Fatalf("Clone = %v, want ErrCloneSpec", err)
			}
		})
	}
}
