package sandbox

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrTreeNotPrepared reports a working tree that still carries something a
// sandbox must never hold: a remote, a credential helper, a reflog, or a
// pointer to git state outside the tree. Create refuses such a tree.
var ErrTreeNotPrepared = errors.New("working tree not prepared for a sandbox")

// minimalConfig replaces .git/config wholesale. Rewriting rather than
// editing means nothing survives that we did not think of: remotes,
// credential helpers, extra headers, includes, external commands.
const minimalConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = false\n"

// strippedPaths are removed from .git outright. Reflogs record the clone
// URL, which is where an installation token ends up when a clone uses an
// authenticated URL; FETCH_HEAD holds the same URL; refs/remotes, remotes
// and branches describe the remote; hooks are executable content nobody
// reviewed.
var strippedPaths = []string{
	"logs", "FETCH_HEAD", "ORIG_HEAD", "refs/remotes", "remotes", "branches", "hooks", "config.worktree",
}

// forbiddenPaths may not exist in a prepared tree. Alternates and commondir
// point at objects or state outside the tree, which a sandbox cannot reach
// and must not be given.
var forbiddenPaths = []string{
	"objects/info/alternates", "commondir", "gitdir",
}

var (
	// forbiddenSection matches config sections that name a remote,
	// credential source, URL rewrite, include, or transport.
	forbiddenSection = regexp.MustCompile(`(?i)^\s*\[\s*(remote|credential|url|include|includeif|http|https|ssh|branch|submodule)\b`)
	// forbiddenKey matches keys that run commands or carry credentials
	// regardless of section.
	forbiddenKey = regexp.MustCompile(`(?i)^\s*(sshcommand|askpass|hookspath|fsmonitor|helper|extraheader|proxy|insteadof|pushinsteadof|gitproxy|editor|pager|external|textconv)\s*=`)
	// packedRemoteRef matches packed-refs lines for remote-tracking refs.
	packedRemoteRef = regexp.MustCompile(`^[0-9a-f]{40,64} refs/remotes/`)
)

// PrepareTree makes a clone safe to copy into a sandbox (FR-SEC21). It
// operates in place: the caller owns the directory and has already cloned
// at the base commit. It is idempotent, so a retried activity can call it
// again on an already prepared tree.
func PrepareTree(treePath string) error {
	gitDir, err := gitDirectory(treePath)
	if err != nil {
		return err
	}
	for _, rel := range forbiddenPaths {
		if _, err := os.Lstat(filepath.Join(gitDir, rel)); err == nil {
			return fmt.Errorf("%w: %s/%s exists", ErrTreeNotPrepared, ".git", rel)
		}
	}
	for _, rel := range strippedPaths {
		if err := os.RemoveAll(filepath.Join(gitDir, rel)); err != nil {
			return fmt.Errorf("removing .git/%s: %w", rel, err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(minimalConfig), 0o644); err != nil {
		return fmt.Errorf("writing .git/config: %w", err)
	}
	if err := stripPackedRemoteRefs(filepath.Join(gitDir, "packed-refs")); err != nil {
		return err
	}
	return VerifyTree(treePath)
}

// VerifyTree reports whether a tree is safe to hand to a sandbox. Providers
// call it in Create as a second line of defence, so a caller that skipped
// PrepareTree cannot leak a remote or a token into a container.
func VerifyTree(treePath string) error {
	gitDir, err := gitDirectory(treePath)
	if err != nil {
		return err
	}
	for _, rel := range append(append([]string{}, forbiddenPaths...), strippedPaths...) {
		if _, err := os.Lstat(filepath.Join(gitDir, rel)); err == nil {
			return fmt.Errorf("%w: .git/%s exists", ErrTreeNotPrepared, rel)
		}
	}
	if err := verifyConfig(filepath.Join(gitDir, "config")); err != nil {
		return err
	}
	return verifyPackedRefs(filepath.Join(gitDir, "packed-refs"))
}

// gitDirectory returns treePath/.git, insisting it is a real directory. A
// .git file means a worktree or submodule whose state lives elsewhere; a
// symlink could point anywhere.
func gitDirectory(treePath string) (string, error) {
	gitDir := filepath.Join(treePath, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrTreeNotPrepared, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: .git is not a directory", ErrTreeNotPrepared)
	}
	return gitDir, nil
}

func verifyConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: reading .git/config: %w", ErrTreeNotPrepared, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if forbiddenSection.MatchString(text) || forbiddenKey.MatchString(text) {
			return fmt.Errorf("%w: .git/config line %d", ErrTreeNotPrepared, line)
		}
	}
	return nil
}

func verifyPackedRefs(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading packed-refs: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if packedRemoteRef.MatchString(line) {
			return fmt.Errorf("%w: packed-refs holds a remote-tracking ref", ErrTreeNotPrepared)
		}
	}
	return nil
}

// stripPackedRemoteRefs drops remote-tracking refs from packed-refs. A
// peeled line (^<sha>) belongs to the ref above it and goes with it.
func stripPackedRemoteRefs(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading packed-refs: %w", err)
	}
	var kept []string
	dropping := false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case packedRemoteRef.MatchString(line):
			dropping = true
			continue
		case dropping && strings.HasPrefix(line, "^"):
			continue
		}
		dropping = false
		kept = append(kept, line)
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		return fmt.Errorf("writing packed-refs: %w", err)
	}
	return nil
}

// HeadCommit resolves HEAD of a prepared tree without invoking git, so the
// worker never runs git against a tree it is about to hand to untrusted
// code. A detached HEAD is returned as is; a symbolic HEAD is followed
// through the loose ref or packed-refs.
func HeadCommit(treePath string) (string, error) {
	gitDir, err := gitDirectory(treePath)
	if err != nil {
		return "", err
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", fmt.Errorf("reading HEAD: %w", err)
	}
	target := strings.TrimSpace(string(head))
	ref, symbolic := strings.CutPrefix(target, "ref: ")
	if !symbolic {
		return validSHA(target)
	}
	if loose, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); err == nil {
		return validSHA(strings.TrimSpace(string(loose)))
	}
	return packedRef(filepath.Join(gitDir, "packed-refs"), ref)
}

func packedRef(path, ref string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", ref, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		sha, name, ok := strings.Cut(line, " ")
		if ok && name == ref {
			return validSHA(sha)
		}
	}
	return "", fmt.Errorf("resolving %s: ref not found", ref)
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func validSHA(s string) (string, error) {
	if !shaPattern.MatchString(s) {
		return "", fmt.Errorf("resolving HEAD: %q is not a commit id", s)
	}
	return s, nil
}
