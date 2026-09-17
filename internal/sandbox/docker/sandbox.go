package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
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
	// ErrFileTooLarge reports a ReadFile of a file larger than
	// Config.MaxOutputBytes. Nothing is killed.
	ErrFileTooLarge = errors.New("file exceeds read limit")
	// ErrKilled reports that processes did not stop when told to and the
	// container was killed outright. The sandbox is unusable afterwards.
	ErrKilled = errors.New("sandbox killed: processes did not stop")
)

// Sandbox is one running container. Commands are serialized: the agent
// runs one at a time, and a file read never races a command. Lifecycle
// state is guarded separately so Close never waits for a command.
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

	// ops serializes commands.
	ops sync.Mutex
	// life guards the fields below.
	life       sync.Mutex
	closed     bool
	closing    bool
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
	s.ops.Lock()
	defer s.ops.Unlock()
	res, err := s.run(ctx, execSpec{cmd: req.Command, workDir: workDir, timeout: req.Timeout})
	if err != nil {
		return res, fmt.Errorf("exec %q: %w", req.Command[0], err)
	}
	return res, nil
}

// Exit codes the file scripts use for their own refusals, distinct from
// anything cat or mkdir return.
const (
	exitSymlink   = 3
	exitNotFile   = 4
	exitMkdir     = 5
	exitTooLarge  = 6
	execArgvShell = "/bin/sh"
)

// readScript walks the path one component at a time, refusing any
// symlink so a read cannot be redirected out of the workspace, then
// prints the file. The path arrives as $1 and is only ever expanded
// quoted, with globbing off, so it is used exactly as given. It runs as
// the sandbox user and so cannot read what the agent could not.
const readScript = `set -f
rest=$1; p=/workspace
while [ -n "$rest" ]; do
  case $rest in
    */*) c=${rest%%/*}; rest=${rest#*/} ;;
    *) c=$rest; rest= ;;
  esac
  p="$p/$c"
  [ -L "$p" ] && { echo "refusing symlink: $p" >&2; exit 3; }
done
[ -f "$p" ] || { echo "not a regular file: $p" >&2; exit 4; }
[ "$(wc -c < "$p")" -le "$2" ] || { echo "file larger than $2 bytes" >&2; exit 6; }
exec cat -- "$p"`

// writeScript mirrors readScript, creating missing parent directories and
// refusing to write through a symlink or over a non-file.
const writeScript = `set -f
rest=$1; p=/workspace
while [ -n "$rest" ]; do
  case $rest in
    */*) c=${rest%%/*}; rest=${rest#*/} ;;
    *) c=$rest; rest= ;;
  esac
  p="$p/$c"
  [ -L "$p" ] && { echo "refusing symlink: $p" >&2; exit 3; }
  [ -n "$rest" ] || break
  [ -e "$p" ] || mkdir -- "$p" || exit 5
  [ -d "$p" ] || { echo "not a directory: $p" >&2; exit 4; }
done
[ -e "$p" ] && [ ! -f "$p" ] && { echo "not a regular file: $p" >&2; exit 4; }
exec cat > "$p"`

// ReadFile returns the contents of a workspace file. A file larger than
// Config.MaxOutputBytes is refused with ErrFileTooLarge before any of it
// is read.
func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	rel, err := workspacePath(path)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	s.ops.Lock()
	defer s.ops.Unlock()
	limit := strconv.FormatInt(s.maxOutput, 10)
	res, err := s.run(ctx, execSpec{cmd: []string{execArgvShell, "-c", readScript, "sh", rel, limit}, workDir: workspaceDir})
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	if err := fileScriptError(res); err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	return res.Stdout, nil
}

// WriteFile replaces the contents of a workspace file, creating parents.
func (s *Sandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	rel, err := workspacePath(path)
	if err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	s.ops.Lock()
	defer s.ops.Unlock()
	res, err := s.run(ctx, execSpec{cmd: []string{execArgvShell, "-c", writeScript, "sh", rel}, workDir: workspaceDir, stdin: bytes.NewReader(data)})
	if err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if err := fileScriptError(res); err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	return nil
}

func fileScriptError(res contracts.ExecResult) error {
	msg := strings.TrimSpace(string(res.Stderr))
	switch res.ExitCode {
	case 0:
		return nil
	case exitSymlink:
		return fmt.Errorf("%w: %s", ErrInvalidPath, msg)
	case exitTooLarge:
		return fmt.Errorf("%w: %s", ErrFileTooLarge, msg)
	case exitNotFile, exitMkdir:
		return errors.New(msg)
	default:
		return fmt.Errorf("exit %d: %s", res.ExitCode, msg)
	}
}

// Diff returns the workspace as a binary-safe unified diff against the
// base commit, new files included. It runs git inside the container as the
// sandbox user, so a .git the agent has tampered with can only mislead the
// agent's own attempt; the worker never runs git over a tree that
// untrusted code has touched.
func (s *Sandbox) Diff(ctx context.Context) ([]byte, error) {
	script := `git add -A -N . && exec git diff --binary --no-color --no-ext-diff "$1"`
	s.ops.Lock()
	defer s.ops.Unlock()
	res, err := s.run(ctx, execSpec{cmd: []string{execArgvShell, "-c", script, "sh", s.base}, workDir: workspaceDir})
	if err != nil {
		return nil, fmt.Errorf("diffing workspace: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("diffing workspace: git exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return res.Stdout, nil
}

// disconnectNetwork ends the install phase: the container loses every
// interface but loopback. CreateWithInstall calls it before returning, so
// no sandbox is ever handed out with a network attached.
func (s *Sandbox) disconnectNetwork(ctx context.Context) error {
	s.life.Lock()
	defer s.life.Unlock()
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

// Close removes the container and its network. It does not wait for a
// running command: force removal ends the command, whose Exec then
// returns ErrClosed. A second call returns nil without touching the
// daemon; a failed call may be retried.
func (s *Sandbox) Close(ctx context.Context) error {
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return nil
	}
	// Mark before removing, so a command the removal interrupts reports
	// ErrClosed rather than a made-up exit status.
	s.closing = true
	if s.id != "" {
		_, err := s.cli.ContainerRemove(ctx, s.id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			s.closing = false
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

func (s *Sandbox) isClosed() bool {
	s.life.Lock()
	defer s.life.Unlock()
	return s.closed || s.closing
}
