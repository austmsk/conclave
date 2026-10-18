package agent

import (
	"encoding/json"
	"testing"
	"time"
)

// TestEveryToolDescribesItself checks that each tool's schema carries a
// top-level description, which the runner sends as the tool's definition.
// A tool without one reaches the model as a bare name.
func TestEveryToolDescribesItself(t *testing.T) {
	env := toolEnv{
		commands:          Commands{Test: []string{"go", "test"}, Build: []string{"go", "build"}, Lint: []string{"go", "vet"}},
		maxCommandTimeout: time.Minute,
	}
	for name, tool := range registry(env) {
		var schema struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
			t.Errorf("%s: schema is not JSON: %v", name, err)
			continue
		}
		if schema.Type != "object" || schema.Description == "" {
			t.Errorf("%s: schema type %q, description %q; want an object with a description", name, schema.Type, schema.Description)
		}
	}
}
