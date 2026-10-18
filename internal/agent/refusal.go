package agent

import (
	"errors"
	"fmt"

	"github.com/austmsk/conclave/internal/sandbox"
)

// Refusal is a tool error the model sees and can correct: bad arguments, a
// denied path, a tool not on the role's list. It is a normal result, not a
// failure of the session. Its message is written by this package, never
// copied from sandbox or provider output.
type Refusal struct {
	// Code is the sanitized name recorded in the audit's error column.
	Code string
	// Msg is what the model is told.
	Msg string
	// Rejected marks a refusal on security grounds (FR-SEC29), counted
	// per attempt by gating.
	Rejected bool
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Msg }

// refuse returns a Refusal the model can correct.
func refuse(code, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// reject returns a Refusal on security grounds.
func reject(code, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Msg: fmt.Sprintf(format, args...), Rejected: true}
}

// Audit codes for sandbox refusals and failures.
const (
	codeInvalidPath    = "invalid_path"
	codeNotFile        = "not_file"
	codeFileTooLarge   = "file_too_large"
	codeOutputTooLarge = "output_too_large"
	codeSandboxError   = "sandbox_error"
)

// outcome is how the runner treats an error from Tool.Execute.
type outcome struct {
	// modelText is what the model sees in ToolResult.Error.
	modelText string
	// auditCode is the sanitized text for the audit's error column.
	auditCode string
	rejected  bool
	// fatal ends the session with sandbox_error; the model sees nothing.
	fatal bool
}

// classify sorts a tool error into a refusal the model sees or a sandbox
// failure that ends the session. A sandbox refusal the backend reports
// through a shared sentinel is shown to the model; any other error is a
// failure, because its text is unvetted and the sandbox may be broken.
func classify(err error) outcome {
	var r *Refusal
	switch {
	case errors.As(err, &r):
		return outcome{modelText: r.Msg, auditCode: r.Code, rejected: r.Rejected}
	case errors.Is(err, sandbox.ErrInvalidPath):
		return outcome{modelText: err.Error(), auditCode: codeInvalidPath, rejected: true}
	case errors.Is(err, sandbox.ErrNotFile):
		return outcome{modelText: err.Error(), auditCode: codeNotFile}
	case errors.Is(err, sandbox.ErrFileTooLarge):
		return outcome{modelText: err.Error(), auditCode: codeFileTooLarge}
	case errors.Is(err, sandbox.ErrOutputTooLarge):
		return outcome{modelText: err.Error(), auditCode: codeOutputTooLarge}
	default:
		return outcome{auditCode: codeSandboxError, fatal: true}
	}
}
