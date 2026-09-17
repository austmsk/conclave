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

// strippedPaths are removed from every git directory in the tree, the
// superproject's and each submodule's under .git/modules. Reflogs record
// the clone URL; FETCH_HEAD holds it too; refs/remotes, remotes and
// branches describe the remote; hooks are executable content nobody
// reviewed.
var strippedPaths = []string{
	"logs", "FETCH_HEAD", "ORIG_HEAD", "refs/remotes", "remotes", "branches", "hooks", "config.worktree",
}

// forbiddenPaths may not exist in a prepared tree. Alternates point at
// objects outside the tree, which a sandbox cannot reach and must not be
// given; http-alternates can carry a credentialed URL as well.
var forbiddenPaths = []string{
	"objects/info/alternates", "objects/info/http-alternates", "commondir", "gitdir",
}

// keptSections are the only config sections a prepared tree may hold.
// core carries the repository format and, for a submodule, its worktree;
// extensions carries the object format. Everything else, remotes and
// credentials included, is dropped.
var keptSections = map[string]bool{"core": true, "extensions": true}

var (
	sectionHeader = regexp.MustCompile(`^\s*\[\s*([^\s\]"]+)`)
	// forbiddenKey matches keys that run commands or carry credentials in
	// any section, including the ones that are kept.
	forbiddenKey = regexp.MustCompile(`(?i)^\s*(sshcommand|askpass|hookspath|fsmonitor|helper|extraheader|proxy|insteadof|pushinsteadof|gitproxy|editor|pager|external|textconv|worktreeconfig)\s*=`)
	// packedRemoteRef matches packed-refs lines for remote-tracking refs.
	packedRemoteRef = regexp.MustCompile(`^[0-9a-f]{40,64} refs/remotes/`)
)

// PrepareTree makes a clone safe to copy into a sandbox (FR-SEC21). It
// operates in place: the caller owns the directory and has already cloned
// at the base commit. It is idempotent, so a retried activity can call it
// again on an already prepared tree.
//
// Clone keeps the installation token out of git entirely, so this is
// defence in depth: it removes what a differently made clone might have
// left, and VerifyTree then proves nothing of the kind remains.
func PrepareTree(treePath string) error {
	dirs, err := gitDirectories(treePath)
	if err != nil {
		return err
	}
	for _, gitDir := range dirs {
		if err := prepareGitDir(gitDir); err != nil {
			return fmt.Errorf("preparing %s: %w", gitDir, err)
		}
	}
	return VerifyTree(treePath)
}

func prepareGitDir(gitDir string) error {
	for _, rel := range forbiddenPaths {
		if _, err := os.Lstat(filepath.Join(gitDir, rel)); err == nil {
			return fmt.Errorf("%w: %s exists", ErrTreeNotPrepared, rel)
		}
	}
	for _, rel := range strippedPaths {
		if err := os.RemoveAll(filepath.Join(gitDir, rel)); err != nil {
			return fmt.Errorf("removing %s: %w", rel, err)
		}
	}
	if err := scrubConfig(filepath.Join(gitDir, "config")); err != nil {
		return err
	}
	return stripPackedRemoteRefs(filepath.Join(gitDir, "packed-refs"))
}

// VerifyTree reports whether a tree is safe to hand to a sandbox. Providers
// call it in Create as a second line of defence, so a caller that skipped
// PrepareTree cannot leak a remote or a token into a container.
func VerifyTree(treePath string) error {
	dirs, err := gitDirectories(treePath)
	if err != nil {
		return err
	}
	for _, gitDir := range dirs {
		if err := verifyGitDir(gitDir); err != nil {
			return fmt.Errorf("%s: %w", gitDir, err)
		}
	}
	return nil
}

func verifyGitDir(gitDir string) error {
	for _, rel := range append(append([]string{}, forbiddenPaths...), strippedPaths...) {
		if _, err := os.Lstat(filepath.Join(gitDir, rel)); err == nil {
			return fmt.Errorf("%w: %s exists", ErrTreeNotPrepared, rel)
		}
	}
	if err := verifyConfig(filepath.Join(gitDir, "config")); err != nil {
		return err
	}
	return verifyPackedRefs(filepath.Join(gitDir, "packed-refs"))
}

// gitDirectories returns the tree's git directory followed by every
// submodule git directory nested under .git/modules, however deep. The
// root must be a real directory: a .git file means a worktree or
// submodule whose state lives elsewhere; a symlink could point anywhere.
func gitDirectories(treePath string) ([]string, error) {
	gitDir := filepath.Join(treePath, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTreeNotPrepared, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: .git is not a directory", ErrTreeNotPrepared)
	}
	dirs := []string{gitDir}
	if err := collectModules(gitDir, &dirs); err != nil {
		return nil, err
	}
	return dirs, nil
}

// collectModules walks gitDir/modules. A directory holding HEAD and
// objects is a submodule's git directory; its own modules are walked in
// turn, and its objects are not.
func collectModules(gitDir string, dirs *[]string) error {
	modules := filepath.Join(gitDir, "modules")
	if _, err := os.Lstat(modules); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(modules, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || path == modules || !isGitDir(path) {
			return nil
		}
		*dirs = append(*dirs, path)
		if err := collectModules(path, dirs); err != nil {
			return err
		}
		return fs.SkipDir
	})
}

func isGitDir(path string) bool {
	head, err := os.Lstat(filepath.Join(path, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	objects, err := os.Lstat(filepath.Join(path, "objects"))
	return err == nil && objects.IsDir()
}

// scrubConfig rewrites a git config keeping only the core and extensions
// sections, minus any key that runs a command or carries a credential.
// Rewriting from what is kept, rather than deleting what is known, means
// nothing survives that was not thought of.
func scrubConfig(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data = nil
	} else if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	var out bytes.Buffer
	keep := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if m := sectionHeader.FindStringSubmatch(line); m != nil {
			keep = keptSections[strings.ToLower(m[1])]
		}
		if keep && !forbiddenKey.MatchString(line) {
			out.WriteString(line + "\n")
		}
	}
	if out.Len() == 0 {
		out.WriteString("[core]\n\trepositoryformatversion = 0\n\tbare = false\n")
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

func verifyConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: reading config: %w", ErrTreeNotPrepared, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if m := sectionHeader.FindStringSubmatch(text); m != nil && !keptSections[strings.ToLower(m[1])] {
			return fmt.Errorf("%w: config line %d: section %q", ErrTreeNotPrepared, line, m[1])
		}
		if forbiddenKey.MatchString(text) {
			return fmt.Errorf("%w: config line %d", ErrTreeNotPrepared, line)
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
	dirs, err := gitDirectories(treePath)
	if err != nil {
		return "", err
	}
	gitDir := dirs[0]
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", fmt.Errorf("reading HEAD: %w", err)
	}
	target := strings.TrimSpace(string(head))
	ref, symbolic := strings.CutPrefix(target, "ref: ")
	if !symbolic {
		return validSHA(target)
	}
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
		return "", fmt.Errorf("resolving HEAD: %q is not a ref", ref)
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
