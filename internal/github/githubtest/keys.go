package githubtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// CanaryToken is the installation token the fake API hands out. It follows
// the canary format, so a leak is unmistakable in any output.
const CanaryToken = "canary_github_installation_token_do_not_leak"

// CanaryWebhookSecret is the webhook secret tests sign deliveries with. It
// is long enough for NewReceiver.
const CanaryWebhookSecret = "canary_github_webhook_secret_do_not_leak_0123456789"

var (
	keyOnce sync.Once
	key     *rsa.PrivateKey
	keyErr  error
)

// Key returns a 2048-bit RSA key generated once per test binary. Keys are
// never checked in: one in the repository would be a real credential to
// anything that trusted it.
func Key(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() { key, keyErr = rsa.GenerateKey(rand.Reader, 2048) })
	if keyErr != nil {
		t.Fatalf("generating App key: %v", keyErr)
	}
	return key
}

// KeyFile writes Key to a file under t.TempDir() in the PKCS#1 PEM form
// GitHub issues, with mode 0600, and returns its path.
func KeyFile(t testing.TB) string {
	t.Helper()
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(Key(t))}
	path := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
