package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

const (
	// jwtBackdate and jwtLifetime set the App JWT's iat and exp. GitHub
	// refuses a JWT that lives longer than ten minutes, and backdating iat
	// absorbs clock drift between this host and GitHub.
	jwtBackdate = 60 * time.Second
	jwtLifetime = 9 * time.Minute

	// tokenRefresh is how much life a cached installation token must have
	// left to be handed out. An activity that takes a token just before it
	// expires would fail part-way through a push.
	tokenRefresh = 10 * time.Minute
	// maxTokenLife caps how long a token is trusted, whatever expiry the
	// reply claims. GitHub's tokens last an hour.
	maxTokenLife = time.Hour

	// maxKeyFile bounds how much of the key file is read. A 4096-bit PEM
	// key is about 3 KiB.
	maxKeyFile = 64 << 10
)

// tokenPermissions is every permission an installation token is asked for,
// and the most a token GitHub returns may hold. It is fixed here so that no
// configuration can widen it: never workflows, never administration
// (FR-SEC15). Checks are read with ticket 9.
var tokenPermissions = map[string]string{
	"contents":      "write",
	"issues":        "write",
	"pull_requests": "write",
	"metadata":      "read",
}

// forbiddenPermissions are refused in a token reply even if a future edit
// adds them to tokenPermissions by mistake.
var forbiddenPermissions = map[string]bool{"workflows": true, "administration": true}

// permissionLevel orders access levels; an unknown level is never within
// a request.
var permissionLevel = map[string]int{"read": 1, "write": 2}

// cachedToken is an installation token and when it stops being usable.
type cachedToken struct {
	value   secret
	expires time.Time
}

// LoadAppKey reads the App's private key from path: a regular file, mode
// 0600 or 0400, holding exactly one PEM block with an RSA key of at least
// 2048 bits, in PKCS#1 or PKCS#8 form. Errors never quote the file.
func LoadAppKey(path string) (*rsa.PrivateKey, error) {
	data, err := readKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: App key %s: %w", ErrInvalidConfig, path, err)
	}
	defer clear(data)
	key, err := parseAppKey(data)
	if err != nil {
		return nil, fmt.Errorf("%w: App key %s: %w", ErrInvalidConfig, path, err)
	}
	return key, nil
}

// readKeyFile reads a key file that is a regular file nobody but its owner
// can read. The file opened must be the one inspected, so a symlink swapped
// in between the two is caught.
func readKeyFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspecting: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("is not a regular file (mode %s)", info.Mode().Type())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("mode %#o lets others read it; want 0600 or 0400", perm)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspecting: %w", err)
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("changed while being opened")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, fmt.Errorf("reading: %w", err)
	}
	if len(data) > maxKeyFile {
		clear(data)
		return nil, fmt.Errorf("is larger than %d bytes", maxKeyFile)
	}
	return data, nil
}

// parseAppKey accepts exactly one unencrypted PEM block and nothing else
// but whitespace. Parse errors are replaced with fixed text: their detail
// describes the key's encoding, and this error reaches logs.
func parseAppKey(data []byte) (*rsa.PrivateKey, error) {
	if !bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte("-----BEGIN ")) {
		return nil, fmt.Errorf("is not a PEM file")
	}
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("is not a PEM file")
	}
	defer clear(block.Bytes)
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("holds more than one PEM block")
	}
	if len(block.Headers) != 0 {
		return nil, fmt.Errorf("PEM block has headers; an encrypted key is not supported")
	}
	key, err := parseRSAKey(block)
	if err != nil {
		return nil, err
	}
	if key.N.BitLen() < minKeyBits {
		return nil, fmt.Errorf("RSA key has %d bits, fewer than %d", key.N.BitLen(), minKeyBits)
	}
	return key, nil
}

func parseRSAKey(block *pem.Block) (*rsa.PrivateKey, error) {
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("PEM block is not a valid PKCS#1 RSA key")
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("PEM block is not a valid PKCS#8 key")
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key is not an RSA key")
		}
		return key, nil
	default:
		return nil, fmt.Errorf("PEM block is not an RSA private key")
	}
}

// appJWT signs the short-lived JWT that authenticates as the App itself.
// It is used only to mint installation tokens.
func (a *App) appJWT(now time.Time) (secret, error) {
	claims, err := json.Marshal(struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}{
		IssuedAt:  now.Add(-jwtBackdate).Unix(),
		ExpiresAt: now.Add(jwtLifetime).Unix(),
		Issuer:    strconv.FormatInt(a.cfg.AppID, 10),
	})
	if err != nil {
		return secret{}, fmt.Errorf("github: encoding JWT claims: %w", err)
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		// The cause names the key's shape at most; it is dropped anyway.
		return secret{}, fmt.Errorf("github: signing App JWT failed")
	}
	return newSecret(signingInput + "." + enc.EncodeToString(sig)), nil
}

// installationToken returns a token for repo, minting one when the cache
// has none with at least ten minutes left. Each token is scoped to that one
// repository and to the fixed permission set, and a token GitHub returns
// with any other permission or repository is discarded unused.
func (a *App) installationToken(ctx context.Context, repo RepoRef) (secret, error) {
	if repo.ID <= 0 {
		return secret{}, fmt.Errorf("%w: repository %q has no numeric ID", ErrRefused, repo.FullName)
	}
	if repo.InstallationID != 0 && repo.InstallationID != a.cfg.InstallationID {
		return secret{}, fmt.Errorf("%w: repository %q belongs to installation %d, not %d",
			ErrRefused, repo.FullName, repo.InstallationID, a.cfg.InstallationID)
	}
	// The lock is held across the mint, so concurrent calls for one
	// repository mint once. One owner's traffic makes this cheap.
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if c, ok := a.tokens[repo.ID]; ok && c.expires.Sub(now) >= tokenRefresh {
		return c.value, nil
	}
	delete(a.tokens, repo.ID)
	tok, err := a.mintToken(ctx, repo.ID, now)
	if err != nil {
		return secret{}, err
	}
	a.tokens[repo.ID] = tok
	return tok.value, nil
}

// invalidate drops repo's cached token, after GitHub rejected it, so the
// next call mints a fresh one.
func (a *App) invalidate(repoID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, repoID)
}

type tokenRequest struct {
	RepositoryIDs []int64           `json:"repository_ids"`
	Permissions   map[string]string `json:"permissions"`
}

type tokenReply struct {
	Token        string            `json:"token"`
	ExpiresAt    string            `json:"expires_at"`
	Permissions  map[string]string `json:"permissions"`
	Repositories []struct {
		ID int64 `json:"id"`
	} `json:"repositories"`
}

// mintToken exchanges the App JWT for an installation token scoped to one
// repository.
func (a *App) mintToken(ctx context.Context, repoID int64, now time.Time) (cachedToken, error) {
	jwt, err := a.appJWT(now)
	if err != nil {
		return cachedToken{}, err
	}
	path := "/app/installations/" + strconv.FormatInt(a.cfg.InstallationID, 10) + "/access_tokens"
	req := tokenRequest{RepositoryIDs: []int64{repoID}, Permissions: tokenPermissions}
	var reply tokenReply
	if err := a.do(ctx, "minting installation token", http.MethodPost, path, jwt, req, &reply); err != nil {
		return cachedToken{}, err
	}
	expires, err := checkTokenReply(reply, repoID, now)
	if err != nil {
		return cachedToken{}, fmt.Errorf("github: installation token discarded: %w", err)
	}
	if limit := now.Add(maxTokenLife); expires.After(limit) {
		expires = limit
	}
	return cachedToken{value: newSecret(reply.Token), expires: expires}, nil
}

// checkTokenReply refuses a token that is broader than the request: any
// permission not asked for or at a higher level, or any repository but
// the one asked for. GitHub should never send one; a token that could
// change workflows or reach another repository is not used even once.
func checkTokenReply(r tokenReply, repoID int64, now time.Time) (time.Time, error) {
	if r.Token == "" {
		return time.Time{}, fmt.Errorf("%w: reply has no token", ErrRefused)
	}
	expires, err := time.Parse(time.RFC3339, r.ExpiresAt)
	if err != nil || !expires.After(now) {
		return time.Time{}, fmt.Errorf("%w: reply has no usable expiry", ErrRefused)
	}
	for name, level := range r.Permissions {
		if err := checkPermission(name, level); err != nil {
			return time.Time{}, err
		}
	}
	if len(r.Repositories) != 1 || r.Repositories[0].ID != repoID {
		return time.Time{}, fmt.Errorf("%w: token is not scoped to exactly repository %d", ErrRefused, repoID)
	}
	return expires, nil
}

func checkPermission(name, level string) error {
	want, asked := tokenPermissions[name]
	if forbiddenPermissions[name] || !asked {
		return fmt.Errorf("%w: token grants %q, which was not requested", ErrRefused, name)
	}
	got, known := permissionLevel[level]
	if !known || got > permissionLevel[want] {
		return fmt.Errorf("%w: token grants %q above %q", ErrRefused, name, want)
	}
	return nil
}
