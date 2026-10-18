package agent

import (
	"encoding/json"
	"errors"

	"github.com/austmsk/conclave/internal/contracts"
)

// finishSchema is the JSON Schema of the finish tool's arguments, built
// from the same definition validateReport enforces.
func finishSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

// validateReport decodes and validates a finish report. Its error message
// is written for the model, which gets exactly one chance to repair.
func validateReport(raw json.RawMessage) (contracts.CompletionReport, error) {
	return contracts.CompletionReport{}, errors.New("not implemented")
}
