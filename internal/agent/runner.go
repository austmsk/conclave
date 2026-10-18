package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/austmsk/conclave/internal/contracts"
)

var _ contracts.AgentRunner = (*Runner)(nil)

// errWallClock is the cause the session's context carries when
// MaxWallClock runs out. It tells the runner's own deadline apart from a
// cancellation or deadline of the caller's, which is an error, not a
// wall_clock outcome.
var errWallClock = errors.New("session wall clock exhausted")

// Run executes one agent session: it sends the task and the offered tools
// to the model, runs the calls the model makes, records each before the
// model sees its result, and repeats until a valid finish or a limit.
//
// The outcomes:
//   - a valid finish returns the report with Reason empty and a nil
//     error;
//   - a limit (steps, tokens, the attempt budget, wall clock, repeated
//     identical failures) or a second invalid report returns its Reason
//     and a nil error: the agent failed, the platform did not;
//   - a sandbox failure returns Reason sandbox_error and an error, and a
//     model call the router refused or failed for any reason but a
//     budget returns Reason provider_error and an error;
//   - the feature request and monthly caps, a cancelled caller context,
//     an invalid configuration, and a failed audit or blob write return
//     an error with no Reason. A cap error still matches
//     llm.ErrBudgetExceeded, so the caller can route it to
//     needs_attention.
//
// The output carries every step taken so far, the usage and the
// provenance, whatever the outcome.
func (r *Runner) Run(ctx context.Context, sb contracts.Sandbox, in contracts.SessionInput) (contracts.SessionOutput, error) {
	s, err := r.newSession(sb, in)
	if err != nil {
		return contracts.SessionOutput{}, err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, in.Spec.Limits.MaxWallClock, errWallClock)
	defer cancel()
	err = s.loop(ctx)
	s.out.Usage.Steps = s.taken
	return s.out, err
}

// newSession checks the role spec against what the runner can offer and
// builds the first request's messages. Nothing reaches the model or the
// sandbox until it passes.
func (r *Runner) newSession(sb contracts.Sandbox, in contracts.SessionInput) (*session, error) {
	if err := checkSpec(in.Spec); err != nil {
		return nil, err
	}
	offered, schemas, err := r.offer(in.Spec)
	if err != nil {
		return nil, err
	}
	return &session{
		r:       r,
		sb:      sb,
		in:      in,
		offered: offered,
		schemas: schemas,
		msgs:    []contracts.Message{taskMessage(in)},
		repeats: map[string]int{},
	}, nil
}

// checkSpec refuses a role spec the runner cannot enforce. Limits are
// enforced here, not requested of the model, so a zero limit would mean
// no limit at all.
func checkSpec(spec contracts.RoleSpec) error {
	l := spec.Limits
	switch {
	case spec.Role == "":
		return fmt.Errorf("%w: role spec has no role", ErrInvalidConfig)
	case spec.PromptVersion == "":
		return fmt.Errorf("%w: role %s has no prompt version", ErrInvalidConfig, spec.Role)
	case l.MaxSteps <= 0, l.MaxTokens <= 0, l.MaxWallClock <= 0, l.MaxRepeatErrors <= 0:
		return fmt.Errorf("%w: role %s limits must all be positive, got steps %d, tokens %d, wall clock %v, repeat errors %d",
			ErrInvalidConfig, spec.Role, l.MaxSteps, l.MaxTokens, l.MaxWallClock, l.MaxRepeatErrors)
	}
	return nil
}

// mutating are the tools a ReadOnly role never gets, whatever its list
// says, so a mistake in a read-only role's list cannot grant writes.
var mutating = map[contracts.ToolName]bool{
	contracts.ToolWriteFile:  true,
	contracts.ToolApplyPatch: true,
	contracts.ToolRunTests:   true,
	contracts.ToolRunBuild:   true,
	contracts.ToolRunLint:    true,
	contracts.ToolRunCommand: true,
}

// offer returns the tools a session is offered: the role's allowlist
// intersected with the registry, in the allowlist's order, less every
// mutating tool for a ReadOnly role. A listed tool the runner cannot
// provide, including a configured command tool whose command is not
// configured, is a configuration error rather than a silent omission,
// and so is a role that cannot finish.
func (r *Runner) offer(spec contracts.RoleSpec) (map[contracts.ToolName]contracts.Tool, []contracts.ToolSchema, error) {
	offered := map[contracts.ToolName]contracts.Tool{}
	var schemas []contracts.ToolSchema
	for _, name := range spec.Tools {
		if _, dup := offered[name]; dup || (spec.ReadOnly && mutating[name]) {
			continue
		}
		tool, ok := r.tools[name]
		if !ok {
			return nil, nil, fmt.Errorf("%w: role %s lists %s, which this runner cannot provide (a command tool needs a configured command)",
				ErrInvalidConfig, spec.Role, name)
		}
		schema, err := toolSchema(tool)
		if err != nil {
			return nil, nil, err
		}
		offered[name] = tool
		schemas = append(schemas, schema)
	}
	if _, ok := offered[contracts.ToolFinish]; !ok {
		return nil, nil, fmt.Errorf("%w: role %s does not allow finish, so no session could complete", ErrInvalidConfig, spec.Role)
	}
	return offered, schemas, nil
}

// toolSchema builds the definition sent to the model. The description is
// the schema's own top-level description, so each tool describes itself
// in one place.
func toolSchema(tool contracts.Tool) (contracts.ToolSchema, error) {
	params := tool.Schema()
	var top struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(params, &top); err != nil {
		return contracts.ToolSchema{}, fmt.Errorf("%w: schema of tool %s: %w", ErrInvalidConfig, tool.Name(), err)
	}
	return contracts.ToolSchema{Name: tool.Name(), Description: top.Description, Parameters: params}, nil
}
