package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
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

// run executes one command and captures its output. Callers hold s.ops.
// However the command ends, every process it started is killed before run
// returns, and so before s.ops is released: nothing a command leaves
// behind can act between it and the next one.
func (s *Sandbox) run(ctx context.Context, spec execSpec) (_ contracts.ExecResult, err error) {
	timeout, err := s.effectiveTimeout(spec.timeout)
	if err != nil {
		return contracts.ExecResult{}, err
	}
	defer func() { err = s.settle(ctx, err) }()
	execID, err := s.cli.ExecCreate(ctx, s.id, client.ExecCreateOptions{
		User:         strconv.Itoa(s.uid) + ":" + strconv.Itoa(s.gid),
		AttachStdin:  spec.stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   spec.workDir,
		Cmd:          spec.cmd,
	})
	if err != nil {
		return contracts.ExecResult{}, s.closedOr(fmt.Errorf("creating exec: %w", err))
	}
	attach, err := s.cli.ExecAttach(ctx, execID.ID, client.ExecAttachOptions{})
	if err != nil {
		return contracts.ExecResult{}, s.closedOr(fmt.Errorf("attaching exec: %w", err))
	}

	start := time.Now()
	output := newCapture(s.maxOutput)
	done := make(chan error, 1)
	go func() { done <- output.consume(attach, spec.stdin) }()

	timedOut, err := s.await(ctx, done, timeout)
	// Whatever path await took, close the stream and wait for the reader
	// before touching the buffers: nothing may still be writing to them.
	attach.Close()
	readErr := <-done

	res := contracts.ExecResult{
		Stdout:   output.stdout.Bytes(),
		Stderr:   output.stderr.Bytes(),
		TimedOut: timedOut,
		Duration: time.Since(start),
	}
	if err != nil {
		return res, err
	}
	if readErr != nil && !timedOut {
		return res, fmt.Errorf("reading output: %w", readErr)
	}
	res.ExitCode, err = s.exitCode(ctx, execID.ID)
	if err == nil && s.isClosed() {
		err = ErrClosed
	}
	return res, s.closedOr(err)
}

// await waits for the stream to end, the timeout to fire, or the caller
// to give up. In the last two cases, and when the output cap is hit, it
// kills the sandbox's processes before returning. It consumes at most one
// value from done and leaves the final one for run.
func (s *Sandbox) await(ctx context.Context, done chan error, timeout time.Duration) (timedOut bool, err error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case readErr := <-done:
		done <- readErr
		if errors.Is(readErr, ErrOutputTooLarge) {
			return false, errors.Join(readErr, s.killAll(ctx))
		}
		return false, nil
	case <-timer.C:
		return true, s.killAll(ctx)
	case <-ctx.Done():
		return false, errors.Join(ctx.Err(), s.killAll(context.WithoutCancel(ctx)))
	}
}

func (s *Sandbox) effectiveTimeout(requested time.Duration) (time.Duration, error) {
	s.life.Lock()
	closed, killed := s.closed, s.killed
	s.life.Unlock()
	if closed {
		return 0, ErrClosed
	}
	if killed {
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

// closedOr reports ErrClosed for a daemon error that arrived because Close
// removed the container under a running command.
func (s *Sandbox) closedOr(err error) error {
	if err != nil && s.isClosed() {
		return ErrClosed
	}
	return err
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

// Kill confirmation. The first look comes quickly so an ordinary command
// pays little for it; later looks back off, and a round that ends with
// processes still present signals again.
const (
	killRounds    = 4
	killPolls     = 6
	firstPoll     = 10 * time.Millisecond
	settleTimeout = 30 * time.Second
)

// initSleep is the command line of the init's own child (see initScript).
const initSleep = "sleep 3600"

// settle runs after every command, however it ended: it kills whatever
// the command left running and folds a failure to do so into the
// command's own error. The caller's cancellation cannot cut it short.
func (s *Sandbox) settle(ctx context.Context, err error) error {
	if s.unusable() {
		return err
	}
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	killErr := s.killAll(kctx)
	switch {
	case killErr == nil:
		return err
	case err == nil:
		return killErr
	case errors.Is(err, ErrClosed) && errors.Is(killErr, ErrClosed):
		// Close removed the container under both; say so once.
		return err
	default:
		return errors.Join(err, killErr)
	}
}

// killAll signals the init to kill every other process, then confirms
// through the daemon that only the init remains. The signal is always
// sent: a process count taken beforehand proves nothing, since a stray
// can kill the init's sleep and take its place in the count. The init
// answers the signal by killing everything, its own sleep included, and
// then starting a new sleep, so confirmation needs a sleep killAll has
// not seen before and nothing else. If the init does not respond (it may
// have been stopped or replaced), the container is killed and the
// sandbox marked dead. Callers hold s.ops.
func (s *Sandbox) killAll(ctx context.Context) error {
	seen, err := s.baseline(ctx)
	if err != nil {
		return err
	}
	for range killRounds {
		if _, err := s.cli.ContainerKill(ctx, s.id, client.ContainerKillOptions{Signal: "SIGUSR1"}); err != nil {
			return s.closedOr(fmt.Errorf("signalling sandbox init: %w", err))
		}
		view, err := s.awaitQuiet(ctx, seen)
		if err != nil {
			return err
		}
		if view.quiet(seen) {
			s.lastSleep = view.sleeps[0]
			return nil
		}
		seen = append(seen, view.sleeps...)
	}
	return s.killContainer(ctx)
}

// baseline returns the init's sleep children as they were before the
// signal: the one the last confirmed kill left or, on the first kill,
// whatever the daemon lists now. It also records the init's PID.
func (s *Sandbox) baseline(ctx context.Context) ([]string, error) {
	if s.initPID == "" {
		insp, err := s.cli.ContainerInspect(ctx, s.id, client.ContainerInspectOptions{})
		if err != nil {
			return nil, s.closedOr(fmt.Errorf("inspecting sandbox init: %w", err))
		}
		if insp.Container.State == nil || insp.Container.State.Pid == 0 {
			return nil, errors.New("sandbox init is not running")
		}
		s.initPID = strconv.Itoa(insp.Container.State.Pid)
	}
	if s.lastSleep != "" {
		return []string{s.lastSleep}, nil
	}
	view, err := s.processes(ctx)
	if err != nil {
		return nil, err
	}
	return view.sleeps, nil
}

// awaitQuiet polls the process list until it is quiet or the round's
// polls run out, and returns the last list seen.
func (s *Sandbox) awaitQuiet(ctx context.Context, seen []string) (procView, error) {
	var view procView
	wait := firstPoll
	for range killPolls {
		select {
		case <-ctx.Done():
			return view, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
		var err error
		if view, err = s.processes(ctx); err != nil {
			return view, err
		}
		if view.quiet(seen) {
			return view, nil
		}
	}
	return view, nil
}

// procView is the container's process list as killAll reads it.
type procView struct {
	initSeen bool
	sleeps   []string // PIDs of the init's children running initSleep
	strays   int      // every other process
}

// quiet reports that the init and exactly one sleep remain, and that the
// sleep is new: the init has run its kill since the signal and restarted.
func (v procView) quiet(seen []string) bool {
	return v.initSeen && v.strays == 0 && len(v.sleeps) == 1 && !slices.Contains(seen, v.sleeps[0])
}

// processes lists the container's processes through the daemon, which
// reports host PIDs and cannot be misled by anything inside the container.
func (s *Sandbox) processes(ctx context.Context) (procView, error) {
	top, err := s.cli.ContainerTop(ctx, s.id, client.ContainerTopOptions{})
	if err != nil {
		return procView{}, s.closedOr(fmt.Errorf("listing sandbox processes: %w", err))
	}
	pid, ppid, cmd := slices.Index(top.Titles, "PID"), slices.Index(top.Titles, "PPID"), slices.Index(top.Titles, "CMD")
	if pid < 0 || ppid < 0 || cmd < 0 {
		return procView{}, fmt.Errorf("listing sandbox processes: unexpected columns %q", top.Titles)
	}
	var v procView
	for _, p := range top.Processes {
		if len(p) != len(top.Titles) {
			return procView{}, fmt.Errorf("listing sandbox processes: malformed row %q", p)
		}
		switch {
		case p[pid] == s.initPID:
			v.initSeen = true
		case p[ppid] == s.initPID && p[cmd] == initSleep:
			v.sleeps = append(v.sleeps, p[pid])
		default:
			v.strays++
		}
	}
	return v, nil
}

// killContainer is killAll's last resort; the sandbox is unusable after.
func (s *Sandbox) killContainer(ctx context.Context) error {
	s.life.Lock()
	s.killed = true
	s.life.Unlock()
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
