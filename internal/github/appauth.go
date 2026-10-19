package github

import (
	"context"
	"crypto/rsa"
	"errors"
	"time"
)

// cachedToken is an installation token and when it stops being usable.
type cachedToken struct {
	value   secret
	expires time.Time
}

// LoadAppKey reads the App's private key from path: a regular file, mode
// 0600 or 0400, holding exactly one PEM block with an RSA key of at least
// 2048 bits, in PKCS#1 or PKCS#8 form. Errors never quote the file.
//
// L0 stub: W2 implements.
func LoadAppKey(path string) (*rsa.PrivateKey, error) {
	_ = path
	return nil, errors.New("github: LoadAppKey not implemented")
}

// installationToken returns a token for repo, minting one when the cache
// has none with at least ten minutes left. Each token is scoped to that one
// repository and to the fixed permission set, and a token GitHub returns
// with any other permission or repository is discarded unused.
//
// L0 stub: W2 implements.
func (a *App) installationToken(ctx context.Context, repo RepoRef) (secret, error) {
	_, _ = ctx, repo
	return secret{}, errors.New("github: installationToken not implemented")
}
