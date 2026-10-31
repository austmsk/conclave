package activity

import (
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

// Helpers exported to the external test package, whose canary test runs
// the workflow package over these activities and so cannot live in this
// package without an import cycle.

// CanaryToolOutput is planted in a file the fake model reads, so it
// passes through a tool result into the agent's transcript.
const CanaryToolOutput = "canary_transcript_output_must_not_enter_history"

// ReadFileTurn is a model turn that reads path.
func ReadFileTurn(path string) faketest.Turn {
	args, _ := json.Marshal(map[string]string{"path": path})
	return faketest.Turn{Response: faketest.Response{
		StopReason: contracts.StopToolUse, InputTokens: 10, OutputTokens: 5,
		ToolCalls: []contracts.ToolCall{{ID: "call-1", Name: contracts.ToolReadFile, Arguments: args}},
	}}
}

// BlobsContain reports whether any file under dir contains s.
func BlobsContain(t *testing.T, dir, s string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found {
			return err
		}
		data, err := os.ReadFile(path)
		found = err == nil && strings.Contains(string(data), s)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Fixture is the package's test fixture, for the external test package.
type Fixture struct{ f *fixture }

// NewFixture builds activities over the test database whose model
// replays turns and whose sandboxes serve files.
func NewFixture(t *testing.T, files map[string]string, turns ...faketest.Turn) Fixture {
	f := newFixture(t, turns...)
	for k, v := range files {
		f.sandboxes.files[k] = v
	}
	return Fixture{f}
}

// Activities returns the activities under test.
func (x Fixture) Activities() *Activities { return x.f.a }

// BlobDir is where the activities store transcripts and tool output.
func (x Fixture) BlobDir() string { return x.f.blobDir }

// Repo is the fixture's repository.
func (x Fixture) Repo() github.RepoRef { return x.f.repoRef() }

// ApprovedTask takes a fresh issue to an approved plan with its task
// created, and returns the task ID and the pinned base commit.
func (x Fixture) ApprovedTask() (taskID, baseSHA string) {
	res, ap := x.f.approved()
	task, err := x.f.a.CreateTask(x.f.ctx, res.PlanID)
	x.f.must(err)
	return task.TaskID, ap.BaseSHA
}

// LiveSandboxes is how many sandboxes the fake provider still holds.
func (x Fixture) LiveSandboxes() int { return x.f.sandboxes.liveIDs() }
