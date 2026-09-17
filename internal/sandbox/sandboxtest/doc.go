// Package sandboxtest builds git repositories for sandbox tests. Every
// repository it creates carries a canary installation token in its remote
// URL and reflog, so a test that copies the tree anywhere can assert the
// token did not travel with it.
package sandboxtest
