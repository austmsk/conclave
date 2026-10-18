package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
)

// Audit codes the runner writes itself.
const (
	codeNotOffered     = "tool_not_offered"
	codeFinishNotAlone = "finish_not_alone"
	codeCancelled      = "cancelled"
	// notExecuted prefixes the code of a call the session ended before
	// running, followed by the reason it ended.
	notExecuted = "not executed: "
)

// writeTimeout bounds one audit or blob write. The writes run detached
// from the session's context, so a call cut short by the wall clock is
// still recorded, but they must not hold a finished session open.
const writeTimeout = 10 * time.Second

// session is one Run's state. It is used from one goroutine.
type session struct {
	r       *Runner
	sb      contracts.Sandbox
	in      contracts.SessionInput
	offered map[contracts.ToolName]contracts.Tool
	schemas []contracts.ToolSchema
	msgs    []contracts.Message
	out     contracts.SessionOutput

	// tokens is the input and output the model has reported so far.
	tokens int
	// step numbers the audit rows; taken counts the calls charged against
	// MaxSteps. A call skipped because the session ended takes a step
	// number but is not charged.
	step  int
	taken int
	// repaired is set once the one repair is used: an invalid finish
	// report, or a reply that made no tool call.
	repaired bool
	// repeats counts failures by repeatKey.
	repeats map[string]int
}

// loop runs turns until one ends the session.
func (s *session) loop(ctx context.Context) error {
	for {
		done, err := s.turn(ctx)
		if done || err != nil {
			return err
		}
	}
}

// end ends the session with reason, as the agent's failure rather than
// the platform's.
func (s *session) end(reason contracts.FailureReason) (bool, error) {
	s.out.Reason = reason
	return true, nil
}

// turn makes one model call and handles its reply.
func (s *session) turn(ctx context.Context) (bool, error) {
	if stop, err := s.interrupted(ctx); stop {
		return true, err
	}
	left := s.in.Spec.Limits.MaxTokens - s.tokens
	if left <= 0 {
		return s.end(contracts.ReasonTokenLimit)
	}
	resp, err := s.r.cfg.Router.Complete(ctx, s.request(left))
	if err != nil {
		return s.modelError(ctx, err)
	}
	s.account(resp)
	s.msgs = append(s.msgs, contracts.Message{Role: contracts.MessageAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
	if s.tokens > s.in.Spec.Limits.MaxTokens {
		// The reply itself went past the limit: none of its calls run.
		return s.stop(ctx, resp.ToolCalls, contracts.ReasonTokenLimit)
	}
	if len(resp.ToolCalls) == 0 {
		return s.nudge()
	}
	return s.execCalls(ctx, resp.ToolCalls)
}

// request builds the next model call. The output limit is what is left
// of the session's tokens; the router lowers it further to fit the model
// and the budgets.
func (s *session) request(left int) contracts.Request {
	return contracts.Request{
		RepoID:        s.in.RepoID,
		Role:          s.in.Spec.Role,
		PromptVersion: s.in.Spec.PromptVersion,
		System:        systemPrompt,
		Messages:      s.msgs,
		Tools:         s.schemas,
		MaxTokens:     left,
	}
}

// account adds a reply's usage to the session's.
func (s *session) account(resp contracts.Response) {
	s.out.Provenance = resp.Provenance
	s.out.Usage.InputTokens += resp.Usage.InputTokens
	s.out.Usage.OutputTokens += resp.Usage.OutputTokens
	s.out.Usage.CostUSD += resp.Usage.CostUSD
	s.tokens += resp.Usage.InputTokens + resp.Usage.OutputTokens
}

// modelError sorts a failed model call. The session's own clock is
// checked first, because a call cut off by it fails with a context error.
// The attempt budget is the agent's token_limit. The feature request and
// monthly caps are not the agent's failure: they return as an error with
// no Reason, still matching llm.ErrBudgetExceeded. Anything else is a
// provider_error, returned as an error too, because the session could not
// go on.
func (s *session) modelError(ctx context.Context, err error) (bool, error) {
	if stop, serr := s.interrupted(ctx); stop {
		return true, serr
	}
	var budget llm.BudgetError
	if errors.As(err, &budget) && budget.Kind == llm.BudgetAttempt {
		return s.end(contracts.ReasonTokenLimit)
	}
	if errors.Is(err, llm.ErrBudgetExceeded) {
		return true, fmt.Errorf("model call for role %s refused by a platform cap: %w", s.in.Spec.Role, err)
	}
	s.out.Reason = contracts.ReasonProviderError
	return true, fmt.Errorf("model call for role %s: %w", s.in.Spec.Role, err)
}

// interrupted reports whether the session's context is done. The
// runner's own wall clock ends the session with wall_clock; the caller's
// cancellation or deadline is an error.
func (s *session) interrupted(ctx context.Context) (bool, error) {
	if ctx.Err() == nil {
		return false, nil
	}
	if errors.Is(context.Cause(ctx), errWallClock) {
		s.out.Reason = contracts.ReasonWallClock
		return true, nil
	}
	return true, fmt.Errorf("agent session stopped: %w", context.Cause(ctx))
}

// nudge answers a reply that made no tool call. The session ends only
// through finish, so the model is told to call it; that uses the one
// repair, and a second such reply, or one after an invalid report, is
// invalid_output.
func (s *session) nudge() (bool, error) {
	if s.repaired {
		return s.end(contracts.ReasonInvalidOutput)
	}
	s.repaired = true
	s.msgs = append(s.msgs, contracts.Message{Role: contracts.MessageUser, Text: finishNudge})
	return false, nil
}

// execCalls runs a reply's calls in order and answers every one. A finish
// is valid only alone: alongside other calls it is refused, and the
// others still run, so the model sees their results and can finish next
// turn. Before each call the clock and the step limit are checked; once
// either ends the session, the rest are recorded as not executed.
func (s *session) execCalls(ctx context.Context, calls []contracts.ToolCall) (bool, error) {
	alone := len(calls) == 1
	results := make([]contracts.ToolResult, 0, len(calls))
	for i, call := range calls {
		if stop, err := s.interrupted(ctx); stop {
			return s.skip(ctx, calls[i:], s.stopCode(err), err)
		}
		if s.taken >= s.in.Spec.Limits.MaxSteps {
			return s.stop(ctx, calls[i:], contracts.ReasonStepLimit)
		}
		res, done, err := s.execOne(ctx, call, alone)
		if err != nil {
			return true, s.abandon(ctx, calls[i+1:], err)
		}
		if done {
			return s.skip(ctx, calls[i+1:], s.stopCode(nil), nil)
		}
		results = append(results, res)
	}
	s.msgs = append(s.msgs, contracts.Message{Role: contracts.MessageUser, ToolResults: results})
	return false, nil
}

// stop ends the session with reason and records calls as not executed.
func (s *session) stop(ctx context.Context, calls []contracts.ToolCall, reason contracts.FailureReason) (bool, error) {
	s.out.Reason = reason
	return s.skip(ctx, calls, notExecuted+string(reason), nil)
}

// abandon records the calls left when a call ended the session with err,
// unless err is a failed audit write, after which nothing can be
// recorded. It returns err, joined with any failure to record.
func (s *session) abandon(ctx context.Context, calls []contracts.ToolCall, err error) error {
	if errors.Is(err, errAudit) {
		return err
	}
	if _, rerr := s.skip(ctx, calls, s.stopCode(err), nil); rerr != nil {
		return errors.Join(err, rerr)
	}
	return err
}

// stopCode is the audit code for calls skipped after the session ended:
// its Reason, or cancelled for a stop the caller caused.
func (s *session) stopCode(err error) string {
	if s.out.Reason == "" && err != nil {
		return notExecuted + codeCancelled
	}
	return notExecuted + string(s.out.Reason)
}

// skip records calls the session will not run, so the audit holds every
// call the model made, and returns the session's end with err.
func (s *session) skip(ctx context.Context, calls []contracts.ToolCall, code string, err error) (bool, error) {
	for _, call := range calls {
		s.step++
		rec := s.newRecord(call)
		rec.Error = code
		rec.OccurredAt = time.Now()
		if rerr := s.record(ctx, rec); rerr != nil {
			return true, rerr
		}
	}
	return true, err
}

// errAudit marks a failed audit or blob write. The session ends at once,
// recording nothing more, because nothing more can be recorded.
var errAudit = errors.New("tool call audit failed")

// execOne runs one call and records it before its result can reach the
// model. It reports whether the call ended the session.
func (s *session) execOne(ctx context.Context, call contracts.ToolCall, alone bool) (contracts.ToolResult, bool, error) {
	s.taken++
	s.step++
	start := time.Now()
	res, toolErr := s.invoke(ctx, call, alone)
	took := time.Since(start)

	var oc outcome
	var stopped bool
	var stopErr error
	if toolErr != nil {
		// A failed call has no output: whatever a tool left in its result
		// alongside an error is unvetted, so it is neither shown nor kept.
		res = contracts.ToolResult{}
		// The clock first: a command cut off by it fails with an error
		// that classify would take for a broken sandbox.
		if stopped, stopErr = s.interrupted(ctx); stopped {
			oc = outcome{auditCode: s.interruptCode()}
		} else {
			oc = classify(toolErr)
		}
	}
	result, err := s.answer(ctx, call, res, oc, took)
	if err != nil {
		return contracts.ToolResult{}, true, err
	}
	switch {
	case stopped:
		return result, true, stopErr
	case oc.fatal:
		s.out.Reason = contracts.ReasonSandboxError
		return result, true, fmt.Errorf("running tool %s: %w", call.Name, toolErr)
	case call.Name == contracts.ToolFinish && alone:
		return result, s.finished(call, oc), nil
	}
	return result, s.repeated(call, res.Output, result), nil
}

// interruptCode is the audit code of a call the session's end cut off.
func (s *session) interruptCode() string {
	if s.out.Reason != "" {
		return string(s.out.Reason)
	}
	return codeCancelled
}

// invoke runs the call's tool, or refuses it. A tool not offered never
// reaches the sandbox, and neither does a finish sent with other calls.
func (s *session) invoke(ctx context.Context, call contracts.ToolCall, alone bool) (contracts.ToolResult, error) {
	tool, ok := s.offered[call.Name]
	if !ok {
		return contracts.ToolResult{}, &Refusal{
			Code: codeNotOffered, Rejected: true,
			Msg: fmt.Sprintf("tool %q is not available in this session; use only the tools you were given", call.Name),
		}
	}
	if call.Name == contracts.ToolFinish && !alone {
		return contracts.ToolResult{}, refuse(codeFinishNotAlone,
			"finish must be the only tool call in its reply; the other calls were run, so read their results and call finish again on its own")
	}
	return tool.Execute(ctx, s.sb, call.Arguments)
}

// answer turns a call's outcome into the model's result and the audit
// row, stores the full output, and records the row. The model sees the
// result only after both writes succeed; either failing ends the attempt,
// because FR-SEC24 requires the trail and the pointer.
func (s *session) answer(ctx context.Context, call contracts.ToolCall, res contracts.ToolResult, oc outcome, took time.Duration) (contracts.ToolResult, error) {
	full := res.Output
	result := contracts.ToolResult{CallID: call.ID, ExitCode: res.ExitCode, Error: oc.modelText}
	result.Output, result.Truncated = truncate(full, s.r.cfg.OutputCap)
	if full != "" {
		key, err := s.store(ctx, full)
		if err != nil {
			return contracts.ToolResult{}, err
		}
		result.ArtifactKey = key
	}
	rec := s.newRecord(call)
	rec.ExitCode = res.ExitCode
	rec.Duration = took
	rec.ArtifactKey = result.ArtifactKey
	rec.Error = oc.auditCode
	rec.Rejected = oc.rejected
	rec.Truncated = result.Truncated
	rec.OccurredAt = time.Now()
	if err := s.record(ctx, rec); err != nil {
		return contracts.ToolResult{}, err
	}
	return result, nil
}

func (s *session) newRecord(call contracts.ToolCall) ToolCallRecord {
	return ToolCallRecord{
		AttemptID:    s.r.cfg.AttemptID,
		SessionKey:   s.r.cfg.SessionKey,
		Step:         s.step,
		CallID:       call.ID,
		Tool:         call.Name,
		ArgumentsRaw: call.Arguments,
	}
}

// store puts a call's full output in the blob store under the
// repository's namespace, detached from the session's context.
func (s *session) store(ctx context.Context, full string) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	key, err := s.r.cfg.Blobs.Put(ctx, s.in.RepoID, []byte(full))
	if err != nil {
		return "", fmt.Errorf("%w: storing output of step %d: %w", errAudit, s.step, err)
	}
	return key, nil
}

// record writes one audit row, detached from the session's context, and
// adds the call to the session's steps.
func (s *session) record(ctx context.Context, rec ToolCallRecord) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := s.r.cfg.Recorder.Record(ctx, rec); err != nil {
		return fmt.Errorf("%w: recording step %d: %w", errAudit, rec.Step, err)
	}
	s.out.Steps = append(s.out.Steps, contracts.ToolCall{ID: rec.CallID, Name: rec.Tool, Arguments: rec.ArgumentsRaw})
	return nil
}

// finished handles a finish sent alone. A valid report ends the session
// with no Reason. The first invalid one is the repair: the model has its
// validation message and another turn. After that, invalid_output.
func (s *session) finished(call contracts.ToolCall, oc outcome) bool {
	if oc.auditCode == "" {
		report, err := validateReport(call.Arguments)
		if err == nil {
			s.out.Report = report
			return true
		}
	}
	if s.repaired {
		s.out.Reason = contracts.ReasonInvalidOutput
		return true
	}
	s.repaired = true
	return false
}

// repeated counts a failed call and reports whether it reached
// MaxRepeatErrors identical failures. full is the untruncated output.
func (s *session) repeated(call contracts.ToolCall, full string, result contracts.ToolResult) bool {
	if result.Error == "" && result.ExitCode == 0 {
		return false
	}
	key := repeatKey(call, full, result)
	s.repeats[key]++
	if s.repeats[key] >= s.in.Spec.Limits.MaxRepeatErrors {
		s.out.Reason = contracts.ReasonRepeatErrors
		return true
	}
	return false
}
