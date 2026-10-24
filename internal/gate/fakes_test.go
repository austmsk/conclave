package gate

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
)

// fakeScanner is a scripted SecretScanner. It returns findings and err on
// every call and records each input it was given.
type fakeScanner struct {
	findings []Finding
	err      error

	mu     sync.Mutex
	inputs [][]byte
}

var _ SecretScanner = (*fakeScanner)(nil)

func (f *fakeScanner) Scan(_ context.Context, input []byte) ([]Finding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, bytes.Clone(input))
	return slices.Clone(f.findings), f.err
}

// calls returns how many times Scan ran.
func (f *fakeScanner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inputs)
}

// lastInput returns the input of the latest Scan, or nil.
func (f *fakeScanner) lastInput() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		return nil
	}
	return f.inputs[len(f.inputs)-1]
}

// resultKey is gate_results' unique key.
type resultKey struct {
	attempt string
	gate    contracts.GateName
	hash    string
}

// fakeResults is an in-memory Results with the store's semantics: a row
// already held under the same (attempt, gate, hash) is kept, and Record
// returns the rows as held, ordered by gate. recordErr and forErr, when
// set, are returned instead.
type fakeResults struct {
	recordErr error
	forErr    error

	mu      sync.Mutex
	rows    map[resultKey]contracts.GateResult
	records int
	nextID  int
}

var _ Results = (*fakeResults)(nil)

func newFakeResults() *fakeResults {
	return &fakeResults{rows: make(map[resultKey]contracts.GateResult)}
}

func (f *fakeResults) Record(_ context.Context, results []contracts.GateResult) ([]contracts.GateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records++
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	if len(results) == 0 {
		return nil, errors.New("fakeResults: no results")
	}
	var out []contracts.GateResult
	for _, r := range results {
		if r.AttemptID != results[0].AttemptID || r.ArtifactHash != results[0].ArtifactHash {
			return nil, errors.New("fakeResults: mixed attempt or artifact in one batch")
		}
		k := resultKey{r.AttemptID, r.Gate, r.ArtifactHash}
		if _, ok := f.rows[k]; !ok {
			f.nextID++
			r.ID = fmt.Sprintf("result-%d", f.nextID)
			f.rows[k] = r
		}
		out = append(out, f.rows[k])
	}
	sortByGate(out)
	return out, nil
}

func (f *fakeResults) ForArtifact(_ context.Context, attemptID, artifactHash string) ([]contracts.GateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forErr != nil {
		return nil, f.forErr
	}
	var out []contracts.GateResult
	for k, r := range f.rows {
		if k.attempt == attemptID && k.hash == artifactHash {
			out = append(out, r)
		}
	}
	sortByGate(out)
	return out, nil
}

// put stores rows directly, bypassing Record and its count, to set up a
// test.
func (f *fakeResults) put(rows ...contracts.GateResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rows {
		f.rows[resultKey{r.AttemptID, r.Gate, r.ArtifactHash}] = r
	}
}

// recordCalls returns how many times Record ran.
func (f *fakeResults) recordCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records
}

// all returns every row held, ordered by attempt, hash and gate.
func (f *fakeResults) all() []contracts.GateResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]contracts.GateResult, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b contracts.GateResult) int {
		return cmp.Or(cmp.Compare(a.AttemptID, b.AttemptID), cmp.Compare(a.ArtifactHash, b.ArtifactHash), cmp.Compare(a.Gate, b.Gate))
	})
	return out
}

func sortByGate(rs []contracts.GateResult) {
	slices.SortFunc(rs, func(a, b contracts.GateResult) int { return cmp.Compare(a.Gate, b.Gate) })
}

// fakeRejections is a RejectionCounter returning a fixed count or error.
type fakeRejections struct {
	count int
	err   error
}

var _ RejectionCounter = fakeRejections{}

func (f fakeRejections) RejectionCount(context.Context, string) (int, error) {
	return f.count, f.err
}

// TestFakeResultsFirstWriterWins keeps the fake honest: it must behave as
// store.GateResults does, or tests built on it prove nothing about RL-2.
func TestFakeResultsFirstWriterWins(t *testing.T) {
	ctx := context.Background()
	hash := strings.Repeat("a", 64)
	f := newFakeResults()
	if _, err := f.Record(ctx, passingRows(w3AttemptID, hash)); err != nil {
		t.Fatal(err)
	}
	flipped := passingRows(w3AttemptID, hash)
	flipped[0].Passed = false
	got, err := f.Record(ctx, flipped)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if !r.Passed || r.ID == "" {
			t.Fatalf("second Record returned %+v, want the first, passing rows", r)
		}
	}
	if n := len(f.all()); n != len(RequiredGates) || f.recordCalls() != 2 {
		t.Errorf("%d rows after %d Record calls, want %d after 2", n, f.recordCalls(), len(RequiredGates))
	}
	if !slices.IsSortedFunc(got, func(a, b contracts.GateResult) int { return cmp.Compare(a.Gate, b.Gate) }) {
		t.Errorf("rows not ordered by gate: %+v", got)
	}
}
