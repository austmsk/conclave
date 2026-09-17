package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/austmsk/conclave/internal/contracts"
)

var (
	// ErrClosed reports use of a sandbox after Close.
	ErrClosed = errors.New("sandbox is closed")
	// ErrExpired reports use of a sandbox past its Limits.Timeout.
	ErrExpired = errors.New("sandbox lifetime exceeded")
	// ErrOutputTooLarge reports a command whose output passed
	// Config.MaxOutputBytes; the command was killed.
	ErrOutputTooLarge = errors.New("command output exceeds limit")
	// ErrKilled reports that processes did not stop when told to and the
	// container was killed outright. The sandbox is unusable afterwards.
	ErrKilled = errors.New("sandbox killed: processes did not stop")
)

// Sandbox is one running container. Operations are serialized: the agent
// runs one command at a time, and a file read never races a command.
type Sandbox struct {
	cli       *client.Client
	id        string
	name      string
	networkID string
	base      string
	limits    contracts.ResourceLimits
	deadline  time.Time
	maxOutput int64
	uid, gid  int

	mu         sync.Mutex
	closed     bool
	killed     bool
	networkCut bool
}

var _ contracts.Sandbox = (*Sandbox)(nil)

// ID returns the container ID.
func (s *Sandbox) ID() string { return s.id }

// BaseCommit is the commit the workspace was copied at; Diff is taken
// against it.
func (s *Sandbox) BaseCommit() string { return s.base }

// Exec runs one command in the workspace. WorkDir is relative to the
// workspace and validated like a file path. A zero Timeout, or one longer
// than the sandbox's own, uses the sandbox's.
func (s *Sandbox) Exec(ctx context.Context, req contracts.ExecRequest) (contracts.ExecResult, error) {
	if len(req.Command) == 0 {
		return contracts.ExecResult{}, errors.New("exec: empty command")
	}
	workDir := workspaceDir
	if req.WorkDir != "" {
		rel, err := workspacePath(req.WorkDir)
		if err != nil {
			return contracts.ExecResult{}, fmt.Errorf("exec workdir: %w", err)
		}
		workDir = workspaceDir + "/" + rel
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.run(ctx, execSpec{cmd: req.Command, workDir: workDir, timeout: req.Timeout})
	if err != nil {
		return res, fmt.Errorf("exec %q: %w", req.Command[0], err)
	}
	return res, nil
}

// readScript walks the path one component at a time, refusing any symlink
// so a read cannot be redirected out of the workspace, then prints the
// file. It runs as the sandbox user and so cannot read what the agent
// could not.
const readScript = `p=/workspace
IFS=/; set -- $1; unset IFS
for c in "$@"; do
  p="$p/$c"
  [ -L "$p" ] && { echo "refusing symlink: $p" >&2; exit 3; }
done
[ -f "$p" ] || { echo "not a regular file: $p" >&2; exit 4; }
exec cat -- "$p"`

// writeScript mirrors readScript, creating missing parent directories and
// refusing to write through a symlink or over a non-file.
const writeScript = `p=/workspace
IFS=/; set -- $1; unset IFS
n=$#; i=0
for c in "$@"; do
  i=$((i+1)); p="$p/$c"
  [ -L "$p" ] && { echo "refusing symlink: $p" >&2; exit 3; }
  [ $i -lt $n ] || break
  [ -e "$p" ] || mkdir -- "$p" || exit 5
  [ -d "$p" ] || { echo "not a directory: $p" >&2; exit 4; }
done
[ -e "$p" ] && [ ! -f "$p" ] && { echo "not a regular file: $p" >&2; exit 4; }
exec cat > "$p"`

// ReadFile returns the contents of a workspace file.
func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	rel, err := workspacePath(path)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.run(ctx, execSpec{cmd: []string{"/bin/sh", "-c", readScript, "sh", rel}, workDir: workspaceDir})
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("reading %q: %s", path, strings.TrimSpace(string(res.Stderr)))
	}
	return res.Stdout, nil
}

// WriteFile replaces the contents of a workspace file, creating parents.
func (s *Sandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	rel, err := workspacePath(path)
	if err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.run(ctx, execSpec{cmd: []string{"/bin/sh", "-c", writeScript, "sh", rel}, workDir: workspaceDir, stdin: bytes.NewReader(data)})
	if err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("writing %q: %s", path, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// Diff returns the workspace as a binary-safe unified diff against the
// base commit, new files included. It runs git inside the container as the
// sandbox user, so a .git the agent has tampered with can only mislead the
// agent's own attempt; the worker never runs git over a tree that
// untrusted code has touched.
func (s *Sandbox) Diff(ctx context.Context) ([]byte, error) {
	script := `git add -A -N . && exec git diff --binary --no-color --no-ext-diff "$1"`
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.run(ctx, execSpec{cmd: []string{"/bin/sh", "-c", script, "sh", s.base}, workDir: workspaceDir})
	if err != nil {
		return nil, fmt.Errorf("diffing workspace: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("diffing workspace: git exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return res.Stdout, nil
}

// DisconnectNetwork ends the install phase: the container loses every
// interface but loopback. It must be called before the agent is given
// control of a sandbox created with NetworkRegistries, and is a no-op for
// one created with NetworkNone or already disconnected.
func (s *Sandbox) DisconnectNetwork(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.networkID == "" || s.networkCut {
		return nil
	}
	_, err := s.cli.NetworkDisconnect(ctx, s.networkID, client.NetworkDisconnectOptions{Container: s.id, Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("disconnecting sandbox %s from its network: %w", s.name, err)
	}
	s.networkCut = true
	return nil
}

// Close removes the container and its network. A second call returns nil
// without touching the daemon; a failed call may be retried.
func (s *Sandbox) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.id != "" {
		_, err := s.cli.ContainerRemove(ctx, s.id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("removing container for sandbox %s: %w", s.name, err)
		}
	}
	if s.networkID != "" {
		_, err := s.cli.NetworkRemove(ctx, s.networkID, client.NetworkRemoveOptions{})
		if err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("removing network for sandbox %s: %w", s.name, err)
		}
	}
	s.closed = true
	return nil
}
