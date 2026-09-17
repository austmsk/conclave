package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ErrCloneSpec reports a CloneSpec the worker refuses to act on.
var ErrCloneSpec = errors.New("invalid clone spec")

// CloneSpec describes a clone into the worker's trusted workspace.
type CloneSpec struct {
	// URL is the plain HTTPS clone URL. It must not carry credentials.
	URL string
	// Dir is the directory to create. It must not exist.
	Dir string
	// Ref is a branch, tag, or full commit id. Empty means the remote's
	// default branch.
	Ref string
	// Token is the installation token, or empty for a public clone. It is
	// handed to git through its environment only, so it is never written to
	// any git config, reflog, or the process command line (FR-SEC21).
	Token string
	// Depth truncates history; 0 keeps it all (FR-SEC17 wants shallow
	// clones in a later milestone).
	Depth int
	// Submodules clones submodules too. They authenticate with the same
	// token, and PrepareTree strips their git directories as well.
	Submodules bool
}

var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Clone clones into spec.Dir and prepares the result for a sandbox. The
// token travels as an Authorization header configured through
// GIT_CONFIG_* environment variables, which git reads like a config file
// but never persists. Nothing of it survives in the tree, and PrepareTree
// afterwards proves so.
func Clone(ctx context.Context, spec CloneSpec) error {
	if err := validateCloneSpec(spec); err != nil {
		return err
	}
	args := []string{"clone", "--quiet", "--no-hardlinks"}
	if spec.Depth > 0 {
		args = append(args, "--depth", strconv.Itoa(spec.Depth))
	}
	if spec.Submodules {
		args = append(args, "--recurse-submodules")
	}
	if spec.Ref != "" && !commitID.MatchString(spec.Ref) {
		args = append(args, "--branch", spec.Ref)
	}
	args = append(args, "--", spec.URL, spec.Dir)
	if err := runGit(ctx, "", spec.Token, args...); err != nil {
		return fmt.Errorf("cloning: %w", err)
	}
	if commitID.MatchString(spec.Ref) {
		if err := checkoutCommit(ctx, spec); err != nil {
			return err
		}
	}
	if err := PrepareTree(spec.Dir); err != nil {
		return fmt.Errorf("preparing clone: %w", err)
	}
	return nil
}

func checkoutCommit(ctx context.Context, spec CloneSpec) error {
	fetch := []string{"fetch", "--quiet", "origin", spec.Ref}
	if spec.Depth > 0 {
		fetch = append(fetch[:2], append([]string{"--depth", strconv.Itoa(spec.Depth)}, fetch[2:]...)...)
	}
	if err := runGit(ctx, spec.Dir, spec.Token, fetch...); err != nil {
		return fmt.Errorf("fetching %s: %w", spec.Ref, err)
	}
	if err := runGit(ctx, spec.Dir, "", "checkout", "--quiet", "--detach", spec.Ref); err != nil {
		return fmt.Errorf("checking out %s: %w", spec.Ref, err)
	}
	if spec.Submodules {
		if err := runGit(ctx, spec.Dir, spec.Token, "submodule", "--quiet", "update", "--init", "--recursive"); err != nil {
			return fmt.Errorf("updating submodules: %w", err)
		}
	}
	return nil
}

func validateCloneSpec(spec CloneSpec) error {
	u, err := url.Parse(spec.URL)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCloneSpec, err)
	}
	if u.User != nil {
		return fmt.Errorf("%w: URL carries credentials", ErrCloneSpec)
	}
	host := u.Hostname()
	loopback := host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	plainToLoopback := u.Scheme == "http" && loopback
	if u.Scheme != "https" && !plainToLoopback {
		return fmt.Errorf("%w: scheme %q is not https", ErrCloneSpec, u.Scheme)
	}
	if spec.Dir == "" {
		return fmt.Errorf("%w: empty directory", ErrCloneSpec)
	}
	if _, err := os.Lstat(spec.Dir); err == nil {
		return fmt.Errorf("%w: %s already exists", ErrCloneSpec, spec.Dir)
	}
	if strings.HasPrefix(spec.Ref, "-") {
		return fmt.Errorf("%w: ref %q looks like an option", ErrCloneSpec, spec.Ref)
	}
	if spec.Depth < 0 {
		return fmt.Errorf("%w: negative depth", ErrCloneSpec)
	}
	return nil
}

// runGit runs git with a scrubbed environment: no system or global
// config, no terminal prompts, and the token, if any, as an extra header
// configured through the environment. Command output is returned in the
// error; git does not echo request headers.
func runGit(ctx context.Context, dir, token string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"LANG=C",
		"LC_ALL=C",
	}
	if token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraheader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth,
		)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(out.String()))
	}
	return nil
}
