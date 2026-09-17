package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/austmsk/conclave/internal/contracts"
)

type execSpec struct {
	cmd     []string
	workDir string
	stdin   io.Reader // nil leaves stdin unattached
	timeout time.Duration
}

// run executes one command and captures its output. Callers hold s.mu.
func (s *Sandbox) run(ctx context.Context, spec execSpec) (contracts.ExecResult, error) {
	timeout, err := s.effectiveTimeout(spec.timeout)
	if err != nil {
		return contracts.ExecResult{}, err
	}
	execID, err := s.cli.ExecCreate(ctx, s.id, client.ExecCreateOptions{
		User:         strconv.Itoa(s.uid) + ":" + strconv.Itoa(s.gid),
		AttachStdin:  spec.stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   spec.workDir,
		Cmd:          spec.cmd,
	})
	if err != nil {
		return contracts.ExecResult{}, fmt.Errorf("creating exec: %w", err)
	}
	attach, err := s.cli.ExecAttach(ctx, execID.ID, client.ExecAttachOptions{})
	if err != nil {
		return contracts.ExecResult{}, fmt.Errorf("attaching exec: %w", err)
	}
	defer attach.Close()

	start := time.Now()
	cap := newCapture(s.maxOutput)
	done := make(chan error, 1)
	go func() { done <- cap.consume(attach, spec.stdin) }()

	res, err := s.await(ctx, done, timeout)
	res.Duration = time.Since(start)
	res.Stdout, res.Stderr = cap.stdout.Bytes(), cap.stderr.Bytes()
	if err != nil {
		return res, err
	}
	res.ExitCode, err = s.exitCode(ctx, execID.ID)
	return res, err
}

// await waits for the stream to end, the timeout to fire, the caller to
// give up, or the output cap to be hit; in every case but the first it
// kills the sandbox's processes before returning.
func (s *Sandbox) await(ctx context.Context, done <-chan error, timeout time.Duration) (contracts.ExecResult, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if errors.Is(err, ErrOutputTooLarge) {
			return contracts.ExecResult{}, errors.Join(err, s.killAll(ctx))
		}
		if err != nil {
			return contracts.ExecResult{}, fmt.Errorf("reading output: %w", err)
		}
		return contracts.ExecResult{}, nil
	case <-timer.C:
		if err := s.killAll(ctx); err != nil {
			return contracts.ExecResult{TimedOut: true}, err
		}
		s.drain(done)
		return contracts.ExecResult{TimedOut: true}, nil
	case <-ctx.Done():
		return contracts.ExecResult{}, errors.Join(ctx.Err(), s.killAll(context.WithoutCancel(ctx)))
	}
}

// drain gives the reader a moment to observe EOF after a kill so the exit
// code is settled; a reader that does not finish is abandoned with the
// connection, which the deferred Close tears down.
func (s *Sandbox) drain(done <-chan error) {
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func (s *Sandbox) effectiveTimeout(requested time.Duration) (time.Duration, error) {
	if s.closed {
		return 0, ErrClosed
	}
	if s.killed {
		return 0, ErrKilled
	}
	remaining := time.Until(s.deadline)
	if remaining <= 0 {
		return 0, ErrExpired
	}
	timeout := requested
	if timeout <= 0 || timeout > s.limits.Timeout {
		timeout = s.limits.Timeout
	}
	return min(timeout, remaining), nil
}

// exitCode polls until the daemon reports the exec finished. The stream
// closes a moment before the exit status is recorded.
func (s *Sandbox) exitCode(ctx context.Context, execID string) (int, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := s.cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return -1, fmt.Errorf("inspecting exec: %w", err)
		}
		if !info.Running {
			return info.ExitCode, nil
		}
		if time.Now().After(deadline) {
			return -1, errors.New("exec still running after its stream closed")
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// initProcesses is what a quiet container shows: the init shell and its
// sleep child.
const initProcesses = 2

// killAll signals the init to kill every other process, then confirms
// through the daemon that only the init remains. If the init does not
// respond (it may have been stopped or replaced), the container is killed
// and the sandbox marked dead.
func (s *Sandbox) killAll(ctx context.Context) error {
	for attempt := 1; attempt <= 6; attempt++ {
		if _, err := s.cli.ContainerKill(ctx, s.id, client.ContainerKillOptions{Signal: "SIGUSR1"}); err != nil {
			return fmt.Errorf("signalling sandbox init: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
		}
		top, err := s.cli.ContainerTop(ctx, s.id, client.ContainerTopOptions{})
		if err != nil {
			return fmt.Errorf("listing sandbox processes: %w", err)
		}
		if len(top.Processes) <= initProcesses {
			return nil
		}
	}
	s.killed = true
	if _, err := s.cli.ContainerKill(ctx, s.id, client.ContainerKillOptions{Signal: "SIGKILL"}); err != nil {
		return fmt.Errorf("%w: and killing the container failed: %w", ErrKilled, err)
	}
	return ErrKilled
}

// capture demultiplexes the exec stream into bounded buffers.
type capture struct {
	stdout, stderr limitedBuffer
}

func newCapture(limit int64) *capture {
	return &capture{stdout: limitedBuffer{limit: limit}, stderr: limitedBuffer{limit: limit}}
}

func (c *capture) consume(attach client.ExecAttachResult, stdin io.Reader) error {
	if stdin != nil {
		go func() {
			// A process that exits before reading everything makes the
			// write fail; that is its exit code's story to tell.
			_, _ = io.Copy(attach.Conn, stdin)
			_ = attach.CloseWrite()
		}()
	}
	_, err := stdcopy.StdCopy(&c.stdout, &c.stderr, attach.Reader)
	return err
}

type limitedBuffer struct {
	bytes.Buffer
	limit int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if int64(b.Len())+int64(len(p)) > b.limit {
		return 0, ErrOutputTooLarge
	}
	return b.Buffer.Write(p)
}
