package activity_test

import (
	"strings"
	"sync"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	sdkactivity "go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/workflow"
)

// recordingConverter is the default data converter that also keeps every
// payload it encodes: workflow inputs and results, activity inputs and
// results, child inputs and results, signals and queries. Everything that
// would be written to workflow history passes through it.
type recordingConverter struct {
	converter.DataConverter
	mu       sync.Mutex
	payloads []string
}

func (c *recordingConverter) record(p *commonpb.Payload) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, string(p.GetData()))
}

func (c *recordingConverter) ToPayload(v any) (*commonpb.Payload, error) {
	p, err := c.DataConverter.ToPayload(v)
	if err == nil {
		c.record(p)
	}
	return p, err
}

func (c *recordingConverter) ToPayloads(vs ...any) (*commonpb.Payloads, error) {
	ps, err := c.DataConverter.ToPayloads(vs...)
	if err == nil {
		for _, p := range ps.GetPayloads() {
			c.record(p)
		}
	}
	return ps, err
}

// TestCanaryNotInHistory runs a task workflow over the real activities
// with a fake model that reads a file holding a canary. The canary
// reaches the transcript in the blob store, and appears in no payload the
// test suite recorded and in no error it returned: transcripts, diffs and
// tool output never enter workflow history (RL-14).
func TestCanaryNotInHistory(t *testing.T) {
	fx := activity.NewFixture(t, map[string]string{"NOTES.md": activity.CanaryToolOutput}, activity.ReadFileTurn("NOTES.md"))
	taskID, base := fx.ApprovedTask()

	rec := &recordingConverter{DataConverter: converter.GetDefaultDataConverter()}
	var errs []string
	var mu sync.Mutex
	env := new(testsuite.WorkflowTestSuite).NewTestWorkflowEnvironment()
	env.SetDataConverter(rec)
	env.SetOnActivityCompletedListener(func(_ *sdkactivity.Info, _ converter.EncodedValue, err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, err.Error())
			mu.Unlock()
		}
	})
	env.RegisterWorkflow(workflow.TaskWorkflow)
	env.RegisterActivity(fx.Activities())
	env.ExecuteWorkflow(workflow.TaskWorkflow, workflow.TaskInput{
		TaskID: taskID, Ordinal: 1, BaseSHA: base, RepoFullName: fx.Repo().FullName, Role: contracts.RoleImplementer,
	})
	if !env.IsWorkflowCompleted() {
		t.Fatal("task workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		errs = append(errs, err.Error())
	}
	if !activity.BlobsContain(t, fx.BlobDir(), activity.CanaryToolOutput) {
		t.Fatal("the canary never reached the transcript, so its absence from history proves nothing")
	}
	if len(rec.payloads) == 0 {
		t.Fatal("no payloads recorded")
	}
	for _, p := range append(rec.payloads, errs...) {
		if strings.Contains(p, "canary_") {
			t.Errorf("history carries the canary: %.200s", p)
		}
	}
	if n := fx.LiveSandboxes(); n != 0 {
		t.Errorf("%d sandboxes left after the task ended", n)
	}
}
