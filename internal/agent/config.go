package agent

import (
	"errors"
	"fmt"
	"time"

	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
)

// ErrInvalidConfig wraps every runner configuration error, including a
// role spec whose limits or tools the runner cannot honour.
var ErrInvalidConfig = errors.New("invalid agent configuration")

// Commands are a repository's configured commands (FR-I4). Each is an argv
// run exactly as given; the model supplies none of it. An empty command
// means the repository has none, and the matching tool is not offered.
type Commands struct {
	Test  []string `yaml:"test"`
	Build []string `yaml:"build"`
	Lint  []string `yaml:"lint"`
}

// Config is one attempt's runner configuration.
type Config struct {
	// Router is scoped to this attempt: in production an *llm.Scoped from
	// llm.Router.Scope with the same AttemptID.
	Router contracts.Router

	Recorder Recorder
	Blobs    blob.Store

	// AttemptID and SessionKey key the audit rows. SessionKey must be
	// unique per Run execution, for example workflow/activity/attempt
	// number, so a retried activity never collides with its predecessor.
	AttemptID  string
	SessionKey string

	Commands Commands

	// Denylist names paths the agent may not touch, matched against whole
	// path components.
	Denylist []string

	// OutputCap is the most bytes of one tool result the model is shown.
	OutputCap int

	// MaxCommandTimeout caps one command tool call.
	MaxCommandTimeout time.Duration
}

// Runner is a contracts.AgentRunner for one attempt.
type Runner struct {
	cfg   Config
	tools map[contracts.ToolName]contracts.Tool
}

// New validates cfg and builds the tool registry.
func New(cfg Config) (*Runner, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	env := toolEnv{denylist: cfg.Denylist, commands: cfg.Commands, maxCommandTimeout: cfg.MaxCommandTimeout}
	return &Runner{cfg: cfg, tools: registry(env)}, nil
}

func (c Config) validate() error {
	switch {
	case c.Router == nil:
		return fmt.Errorf("%w: no router", ErrInvalidConfig)
	case c.Recorder == nil:
		return fmt.Errorf("%w: no recorder", ErrInvalidConfig)
	case c.Blobs == nil:
		return fmt.Errorf("%w: no blob store", ErrInvalidConfig)
	case c.AttemptID == "":
		return fmt.Errorf("%w: no attempt ID", ErrInvalidConfig)
	case c.SessionKey == "":
		return fmt.Errorf("%w: no session key", ErrInvalidConfig)
	case c.OutputCap <= 0:
		return fmt.Errorf("%w: output cap %d must be positive", ErrInvalidConfig, c.OutputCap)
	case c.MaxCommandTimeout <= 0:
		return fmt.Errorf("%w: max command timeout %v must be positive", ErrInvalidConfig, c.MaxCommandTimeout)
	}
	return nil
}
