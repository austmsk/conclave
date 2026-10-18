package agenttest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

// Call is one logged sandbox call. Op is "exec", "read", "write", or
// "diff".
type Call struct {
	Op      string
	Path    string
	Command []string
}

// Sandbox is a contracts.Sandbox over a temporary directory. File
// operations go through an os.Root handle after sandbox.CheckPath, as a
// real backend's do. Exec runs nothing unless ExecFunc is set.
type Sandbox struct {
	// Dir is the workspace on the host, for seeding files and checking
	// results.
	Dir string

	// ExecFunc answers Exec. When nil, Exec returns ErrNoExec.
	ExecFunc func(ctx context.Context, req contracts.ExecRequest) (contracts.ExecResult, error)

	// MaxRead mirrors a backend's output limit: a larger file fails with
	// sandbox.ErrFileTooLarge. Zero means no limit.
	MaxRead int

	root *os.Root

	mu    sync.Mutex
	calls []Call
}

// ErrNoExec is returned by Exec when no ExecFunc is set.
var ErrNoExec = errors.New("agenttest: Exec called with no ExecFunc")

var _ contracts.Sandbox = (*Sandbox)(nil)

// NewSandbox returns a sandbox over a fresh temporary directory.
func NewSandbox(t *testing.T) *Sandbox {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return &Sandbox{Dir: dir, root: root}
}

// Calls returns every call made so far, in order.
func (s *Sandbox) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

func (s *Sandbox) log(c Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
}

// ID implements contracts.Sandbox.
func (s *Sandbox) ID() string { return "agenttest" }

// Exec implements contracts.Sandbox.
func (s *Sandbox) Exec(ctx context.Context, req contracts.ExecRequest) (contracts.ExecResult, error) {
	s.log(Call{Op: "exec", Command: append([]string(nil), req.Command...)})
	if s.ExecFunc == nil {
		return contracts.ExecResult{}, ErrNoExec
	}
	return s.ExecFunc(ctx, req)
}

// ReadFile implements contracts.Sandbox.
func (s *Sandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	s.log(Call{Op: "read", Path: path})
	if err := s.check(path); err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	info, err := s.root.Lstat(path)
	if err != nil {
		return nil, fileError("reading", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("reading %q: %w", path, sandbox.ErrNotFile)
	}
	if s.MaxRead > 0 && info.Size() > int64(s.MaxRead) {
		return nil, fmt.Errorf("reading %q: %w", path, sandbox.ErrFileTooLarge)
	}
	data, err := s.root.ReadFile(path)
	if err != nil {
		return nil, fileError("reading", path, err)
	}
	return data, nil
}

// WriteFile implements contracts.Sandbox.
func (s *Sandbox) WriteFile(_ context.Context, path string, data []byte) error {
	s.log(Call{Op: "write", Path: path})
	if err := s.check(path); err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if dir := parent(path); dir != "" {
		if err := s.root.MkdirAll(dir, 0o755); err != nil {
			return fileError("writing", path, err)
		}
	}
	if err := s.root.WriteFile(path, data, 0o644); err != nil {
		return fileError("writing", path, err)
	}
	return nil
}

// Diff implements contracts.Sandbox. The fake has no git; it returns
// nothing.
func (s *Sandbox) Diff(context.Context) ([]byte, error) {
	s.log(Call{Op: "diff"})
	return nil, nil
}

// Close implements contracts.Sandbox.
func (s *Sandbox) Close(context.Context) error { return nil }

// check applies sandbox.CheckPath, then refuses a symlink at any
// existing component, as the Docker backend's file scripts do.
func (s *Sandbox) check(path string) error {
	if err := sandbox.CheckPath(path); err != nil {
		return err
	}
	for i := 0; i <= len(path); i++ {
		if i < len(path) && path[i] != '/' {
			continue
		}
		info, err := s.root.Lstat(path[:i])
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: refusing symlink: %s", sandbox.ErrInvalidPath, path[:i])
		}
	}
	return nil
}

// fileError maps a missing file to ErrNotFile and anything os.Root
// refuses, such as an escape, to ErrInvalidPath.
func fileError(op, path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s %q: %w", op, path, sandbox.ErrNotFile)
	}
	return fmt.Errorf("%s %q: %w: %w", op, path, sandbox.ErrInvalidPath, err)
}

func parent(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}
