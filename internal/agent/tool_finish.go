package agent

import (
	"context"
	"encoding/json"

	"github.com/austmsk/conclave/internal/contracts"
)

// codeInvalidReport is the audit code of a finish call whose report fails
// validation.
const codeInvalidReport = "invalid_report"

// finish is the finish tool: it ends the session with a completion report.
// It only validates; the runner decides what an accepted or refused report
// means for the session, including whether a repair is left.
type finish struct{}

func newFinish() contracts.Tool { return &finish{} }

func (t *finish) Name() contracts.ToolName { return contracts.ToolFinish }

func (t *finish) Schema() json.RawMessage { return finishSchema() }

// Execute validates the report. An invalid one is a refusal carrying the
// validation message, so the model can see what to fix.
func (t *finish) Execute(_ context.Context, _ contracts.Sandbox, args json.RawMessage) (contracts.ToolResult, error) {
	if _, err := validateReport(args); err != nil {
		return contracts.ToolResult{}, refuse(codeInvalidReport, "%s", err.Error())
	}
	return contracts.ToolResult{Output: "report accepted"}, nil
}
