package gate

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
)

// testRepoID is the repository every test stores blobs under: a UUID as
// Postgres prints it, which blob namespaces require.
const testRepoID = "0b6f3a52-4c1e-4f7a-9d2b-8e5c1a7f3d90"

// newTestBlobs returns a blob store over a fresh temporary directory.
func newTestBlobs(t *testing.T) blob.Store {
	t.Helper()
	d, err := blob.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// errFakeClosed is what a fakeSandbox returns once closed.
var errFakeClosed = errors.New("fake sandbox closed")

// fakeSandbox implements contracts.Sandbox in memory. Every call is
// recorded. Exec answers through the exec hook (exit 0 with no output when
// nil); Diff answers from the diffs queue.
type fakeSandbox struct {
	id string

	// exec answers each Exec. It runs with the fake's lock released, so
	// it may inspect the fake.
	exec func(req contracts.ExecRequest) (contracts.ExecResult, error)

	// writeErr, when set, fails every WriteFile without recording it.
	writeErr error

	// diffs answers Diff in order. The last entry is sticky: it answers
	// every call once the others are used, so two reads of one scripted
	// diff agree. An empty queue answers with diffErr, or an error.
	diffs   [][]byte
	diffErr error

	mu sync.Mutex
	// execs is every Exec request, in order.
	execs []contracts.ExecRequest
	// writes is the last data written to each path; writeOrder is every
	// WriteFile path, in order.
	writes     map[string][]byte
	writeOrder []string
	diffCalls  int
	closed     bool
	closeCalls int
}

var _ contracts.Sandbox = (*fakeSandbox)(nil)

func newFakeSandbox(id string) *fakeSandbox {
	return &fakeSandbox{id: id, writes: map[string][]byte{}}
}

func (f *fakeSandbox) ID() string { return f.id }

func (f *fakeSandbox) Exec(ctx context.Context, req contracts.ExecRequest) (contracts.ExecResult, error) {
	f.mu.Lock()
	f.execs = append(f.execs, req)
	closed := f.closed
	hook := f.exec
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return contracts.ExecResult{}, err
	}
	if closed {
		return contracts.ExecResult{}, errFakeClosed
	}
	if hook == nil {
		return contracts.ExecResult{}, nil
	}
	return hook(req)
}

func (f *fakeSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.writes[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (f *fakeSandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errFakeClosed
	}
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes[path] = append([]byte(nil), data...)
	f.writeOrder = append(f.writeOrder, path)
	return nil
}

func (f *fakeSandbox) Diff(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diffCalls++
	switch {
	case f.closed:
		return nil, errFakeClosed
	case len(f.diffs) == 0 && f.diffErr != nil:
		return nil, f.diffErr
	case len(f.diffs) == 0:
		return nil, errors.New("fake sandbox: no diff scripted")
	}
	d := f.diffs[0]
	if len(f.diffs) > 1 {
		f.diffs = f.diffs[1:]
	}
	return append([]byte(nil), d...), nil
}

func (f *fakeSandbox) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.closeCalls++
	return nil
}

// commands returns every Exec command so far, each joined with spaces.
func (f *fakeSandbox) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.execs))
	for _, req := range f.execs {
		out = append(out, strings.Join(req.Command, " "))
	}
	return out
}

// ran reports the index of the first Exec whose joined command contains
// substr, or -1.
func (f *fakeSandbox) ran(substr string) int {
	for i, c := range f.commands() {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}
