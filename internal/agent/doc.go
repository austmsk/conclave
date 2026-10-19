// Package agent runs one agent session against a sandbox: the loop that
// sends a role's task and tools to the model, runs the tool calls it
// makes, audits each, and ends on finish or a limit (contracts.AgentRunner).
// It also holds the tools an implementer can call: read_file, list_dir,
// search, write_file, apply_patch, run_tests, run_build, run_lint,
// run_command, and finish.
//
// # Wiring
//
// New takes one attempt's Config. The caller builds the Router: in ticket
// 8 the activity calls llm.Router.Scope with the same AttemptID as
// Config.AttemptID and the feature request's ID, and passes the resulting
// *llm.Scoped, which satisfies contracts.Router (*llm.Router does not).
// The scope carries the attempt's token budget; the role's Limits are
// enforced here, on top of it.
//
// SessionKey must be unique per Run execution, for example the workflow
// ID, activity ID and activity attempt number, so a retried activity's
// audit rows never collide with its predecessor's.
//
// # Who decides what
//
//   - Tools validate their own arguments and paths (FR-SEC29) and return
//     full output. What the model should see and correct is a *Refusal,
//     or an error wrapping a sandbox sentinel (sandbox.ErrInvalidPath and
//     the rest); classify in refusal.go sorts these. Any other tool error
//     is a broken sandbox: the session ends with sandbox_error.
//   - The runner decides what is offered (the role's allowlist, less
//     mutating tools for a read-only role), refuses calls to anything
//     else before the sandbox is reached, enforces the limits, truncates
//     output for the model, stores the full output in the blob store,
//     and writes the audit row before the model sees a result. A failed
//     audit or blob write ends the attempt (fail closed, FR-SEC24).
//   - The runner sorts how a session ended, through Reason and the
//     returned error (see Runner.Run). The caller maps that to the
//     attempt's state: a Reason with a nil error is the agent's failure,
//     an error is the platform's.
//
// Raw router errors and unvetted sandbox output, either of which may hold
// a credential, stay out of three places:
//   - The audit's error column holds a sanitized code only.
//   - The model sees refusal text: this package's own, or, for an error
//     wrapping a sandbox sentinel, the backend's message, which its file
//     scripts write about the agent's own, already validated path. Any
//     other tool error ends the session and the model sees nothing.
//   - Run's error is a SessionError: its message is written by this
//     package and safe to persist, and the raw cause is reachable only
//     through errors.Is and errors.As.
//
// Every path a tool touches is checked lexically and refused, never
// sanitized; the sandbox backend then refuses symlinks and hardlinks.
package agent
