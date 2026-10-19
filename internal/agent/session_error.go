package agent

import "fmt"

// SessionError is every error Run returns once a session has started. Its
// text is written by this package from values it controls (role and tool
// names, step numbers, budget kinds), so a caller may persist it: a
// Temporal activity's error message lands in workflow history. The cause
// it wraps may carry raw router or sandbox text, which can hold a
// credential; it is reachable only through errors.Is and errors.As, so
// errors.Is(err, llm.ErrBudgetExceeded) and the like still work.
type SessionError struct {
	msg   string
	cause error
}

func (e *SessionError) Error() string { return e.msg }

func (e *SessionError) Unwrap() error { return e.cause }

// sessionErr returns a SessionError whose text is the formatted message
// and whose cause is cause. Only platform-written values go in args.
func sessionErr(cause error, format string, args ...any) error {
	return &SessionError{msg: fmt.Sprintf(format, args...), cause: cause}
}
