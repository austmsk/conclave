package github_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/github/githubtest"
)

func pemOf(t *testing.T, typ string, der []byte, err error) []byte {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func writeKeyFile(t *testing.T, data []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// keyFileCases builds each case's file at run time; no key is checked in.
func keyFileCases(t *testing.T) map[string]struct {
	path    string
	content []byte
	ok      bool
} {
	t.Helper()
	key := githubtest.Key(t)
	pkcs1 := pemOf(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key), nil)
	pkcs8Der, err := x509.MarshalPKCS8PrivateKey(key)
	pkcs8 := pemOf(t, "PRIVATE KEY", pkcs8Der, err)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDer, err := x509.MarshalPKCS8PrivateKey(ec)
	ecPEM := pemOf(t, "PRIVATE KEY", ecDer, err)
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	smallPEM := pemOf(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(small), nil)
	link := filepath.Join(t.TempDir(), "link.pem")
	if err := os.Symlink(writeKeyFile(t, pkcs1, 0o600), link); err != nil {
		t.Fatal(err)
	}
	garbage := []byte("canary_this_is_not_a_key_at_all\n")
	trailing := append(append([]byte{}, pkcs1...), []byte("canary_trailing_text\n")...)

	type c = struct {
		path    string
		content []byte
		ok      bool
	}
	return map[string]c{
		"PKCS#1 0600":    {writeKeyFile(t, pkcs1, 0o600), pkcs1, true},
		"PKCS#8 0400":    {writeKeyFile(t, pkcs8, 0o400), pkcs8, true},
		"mode 0644":      {writeKeyFile(t, pkcs1, 0o644), pkcs1, false},
		"mode 0640":      {writeKeyFile(t, pkcs1, 0o640), pkcs1, false},
		"directory":      {t.TempDir(), nil, false},
		"symlink":        {link, pkcs1, false},
		"missing":        {filepath.Join(t.TempDir(), "absent.pem"), nil, false},
		"EC key":         {writeKeyFile(t, ecPEM, 0o600), ecPEM, false},
		"1024-bit key":   {writeKeyFile(t, smallPEM, 0o600), smallPEM, false},
		"two PEM blocks": {writeKeyFile(t, append(append([]byte{}, pkcs1...), pkcs8...), 0o600), pkcs8, false},
		"trailing text":  {writeKeyFile(t, trailing, 0o600), trailing, false},
		"garbage":        {writeKeyFile(t, garbage, 0o600), garbage, false},
	}
}

func TestLoadAppKey(t *testing.T) {
	for name, tc := range keyFileCases(t) {
		t.Run(name, func(t *testing.T) {
			key, err := github.LoadAppKey(tc.path)
			if tc.ok {
				if err != nil || key.N.Cmp(githubtest.Key(t).N) != 0 {
					t.Fatalf("got %v, want the App key", err)
				}
				return
			}
			if !errors.Is(err, github.ErrInvalidConfig) {
				t.Fatalf("got %v, want ErrInvalidConfig", err)
			}
			// No line of the file may appear in the error.
			for _, line := range strings.Split(string(tc.content), "\n") {
				if len(line) >= 8 && !strings.HasPrefix(line, "-----") && strings.Contains(err.Error(), line) {
					t.Fatalf("error quotes the file: %v", err)
				}
			}
		})
	}
}

func TestInstallationTokenObtainedAndUsed(t *testing.T) {
	api, app := restFixture(t, restOptions(), nil)
	if _, err := app.CreateComment(t.Context(), restRepo, 7, "status"); err != nil {
		t.Fatal(err)
	}

	mints := api.RequestsTo("/app/")
	if len(mints) != 1 || mints[0].Method != http.MethodPost || mints[0].Path != "/app/installations/77/access_tokens" {
		t.Fatalf("token exchange requests %+v", mints)
	}
	want := `{"repository_ids":[101],"permissions":{"checks":"read","contents":"write","issues":"write","pull_requests":"write","metadata":"read"}}`
	if !sameJSON(t, mints[0].Body, []byte(want)) {
		t.Fatalf("token request body %s, want %s", mints[0].Body, want)
	}
	if strings.Contains(mints[0].Authorization(), githubtest.CanaryToken) {
		t.Fatal("the token exchange carried an installation token")
	}

	calls := api.RequestsTo("/repos/")
	if len(calls) != 1 || calls[0].Authorization() != "Bearer "+githubtest.CanaryToken {
		t.Fatalf("repository requests %+v", calls)
	}
	h := calls[0].Header
	if h.Get("Accept") != "application/vnd.github+json" || h.Get("X-GitHub-Api-Version") != "2022-11-28" || h.Get("User-Agent") != "conclave" {
		t.Fatalf("headers %v", h)
	}
}

func TestTokenWithExcessPermissionsRefused(t *testing.T) {
	cases := []struct {
		name string
		set  func(*githubtest.APIOptions)
	}{
		{"workflows write", func(o *githubtest.APIOptions) { o.ExtraPermissions = map[string]string{"workflows": "write"} }},
		{"workflows read", func(o *githubtest.APIOptions) { o.ExtraPermissions = map[string]string{"workflows": "read"} }},
		{"administration", func(o *githubtest.APIOptions) { o.ExtraPermissions = map[string]string{"administration": "read"} }},
		{"contents admin", func(o *githubtest.APIOptions) { o.ExtraPermissions = map[string]string{"contents": "admin"} }},
		{"metadata write", func(o *githubtest.APIOptions) { o.ExtraPermissions = map[string]string{"metadata": "write"} }},
		// checks is requested read-only (decision 0014); write is wider.
		{"checks write", func(o *githubtest.APIOptions) { o.ExtraPermissions = map[string]string{"checks": "write"} }},
		{"unrequested statuses", func(o *githubtest.APIOptions) { o.ExtraPermissions = map[string]string{"statuses": "read"} }},
		{"extra repository", func(o *githubtest.APIOptions) { o.ExtraRepositoryIDs = []int64{202} }},
		{"missing repositories", func(o *githubtest.APIOptions) { o.OmitRepositories = true }},
		{"unparseable expiry", func(o *githubtest.APIOptions) { o.ExpiresAt = "tomorrow" }},
		{"expired", func(o *githubtest.APIOptions) { o.ExpiresAt = "2001-01-01T00:00:00Z" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := restOptions()
			tc.set(&opts)
			api, app := restFixture(t, opts, nil)
			for range 2 {
				_, err := app.CreateComment(t.Context(), restRepo, 7, "status")
				if !errors.Is(err, github.ErrRefused) {
					t.Fatalf("got %v, want ErrRefused", err)
				}
			}
			if n := len(api.RequestsTo("/repos/")); n != 0 {
				t.Fatalf("the refused token was used in %d requests", n)
			}
			if api.Mints() != 2 {
				t.Fatalf("mints %d, want 2: a refused token must not be cached", api.Mints())
			}
		})
	}
}

func TestTokenCachedUntilNearExpiry(t *testing.T) {
	// A token lasting three hours is still trusted for one.
	for _, ttl := range []time.Duration{time.Hour, 3 * time.Hour} {
		t.Run(ttl.String(), func(t *testing.T) {
			clock := newRESTClock()
			opts := restOptions()
			opts.Now, opts.TokenTTL = clock.Now, ttl
			api, app := restFixture(t, opts, nil)
			github.SetClock(app, clock.Now)

			steps := []struct {
				advance time.Duration
				mints   int
			}{{0, 1}, {0, 1}, {49 * time.Minute, 1}, {2 * time.Minute, 2}, {time.Minute, 2}}
			for i, s := range steps {
				clock.Advance(s.advance)
				if _, err := app.CreateComment(t.Context(), restRepo, 7, "status"); err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if api.Mints() != s.mints {
					t.Fatalf("step %d: mints %d, want %d", i, api.Mints(), s.mints)
				}
			}
		})
	}
}

func TestUnauthorizedInvalidatesToken(t *testing.T) {
	cases := []struct {
		status int
		mints  int
	}{
		{401, 2}, // the rejected token is dropped and the next call mints
		{404, 1},
		{500, 1},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			if _, err := app.CreateComment(t.Context(), restRepo, 7, "first"); err != nil {
				t.Fatal(err)
			}
			api.FailNext("", "/repos/", tc.status, nil, "")
			if _, err := app.CreateComment(t.Context(), restRepo, 7, "second"); err == nil {
				t.Fatal("injected failure did not fail the call")
			}
			if _, err := app.CreateComment(t.Context(), restRepo, 7, "third"); err != nil {
				t.Fatal(err)
			}
			if api.Mints() != tc.mints {
				t.Fatalf("mints %d, want %d", api.Mints(), tc.mints)
			}
		})
	}
}

func TestTokenScopedToOneRepository(t *testing.T) {
	other := github.RepoRef{ID: 202, FullName: "octo/other", InstallationID: restInstallationID}
	api, app := restFixture(t, restOptions(), nil)
	for _, repo := range []github.RepoRef{restRepo, other, restRepo, other} {
		if _, err := app.CreateComment(t.Context(), repo, 7, "status"); err != nil {
			t.Fatal(err)
		}
	}
	mints := api.RequestsTo("/app/")
	if len(mints) != 2 {
		t.Fatalf("mints %d, want one per repository", len(mints))
	}
	for i, id := range []int64{101, 202} {
		var body struct {
			RepositoryIDs []int64 `json:"repository_ids"`
		}
		if err := json.Unmarshal(mints[i].Body, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != id {
			t.Fatalf("mint %d asked for repositories %v, want [%d]", i, body.RepositoryIDs, id)
		}
	}
}

func TestTokenRefusesUnscopedRepository(t *testing.T) {
	cases := map[string]github.RepoRef{
		"no numeric ID":        {FullName: "octo/tally", InstallationID: restInstallationID},
		"another installation": {ID: 101, FullName: "octo/tally", InstallationID: 99},
	}
	for name, repo := range cases {
		t.Run(name, func(t *testing.T) {
			api, app := restFixture(t, restOptions(), nil)
			if _, err := app.CreateComment(t.Context(), repo, 7, "status"); !errors.Is(err, github.ErrRefused) {
				t.Fatalf("got %v, want ErrRefused", err)
			}
			if n := len(api.Requests()); n != 0 {
				t.Fatalf("%d requests sent", n)
			}
		})
	}
}
