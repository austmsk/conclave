package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

// Stand-ins for the backend's errors. gate never imports a backend, so
// classification cannot depend on which one it is.
var (
	errBackendKilled  = errors.New("sandbox killed: processes did not stop")
	errBackendClosed  = errors.New("sandbox is closed")
	errBackendExpired = errors.New("sandbox lifetime exceeded")
)

func TestClassifyExec(t *testing.T) {
	tests := []struct {
		name       string
		res        contracts.ExecResult
		err        error
		wantDetail string
		wantEnv    bool
	}{
		{name: "passed", res: contracts.ExecResult{}},
		{name: "non-zero exit", res: contracts.ExecResult{ExitCode: 1}, wantDetail: "exit 1"},
		{name: "exit 2", res: contracts.ExecResult{ExitCode: 2}, wantDetail: "exit 2"},
		{name: "signalled", res: contracts.ExecResult{ExitCode: 137}, wantDetail: "exit 137"},
		{name: "timed out", res: contracts.ExecResult{TimedOut: true, ExitCode: -1, Duration: 90 * time.Second}, wantDetail: "timed out after 1m30s"},
		{name: "output past limit", err: fmt.Errorf("exec %q: %w", "sh", sandbox.ErrOutputTooLarge), wantDetail: "output exceeded limit"},
		{name: "not executable", res: contracts.ExecResult{ExitCode: 126}, wantEnv: true},
		{name: "not found", res: contracts.ExecResult{ExitCode: 127}, wantEnv: true},
		{name: "killed", err: fmt.Errorf("exec %q: %w", "sh", errBackendKilled), wantEnv: true},
		{name: "closed", err: errBackendClosed, wantEnv: true},
		{name: "expired", err: errBackendExpired, wantEnv: true},
		{name: "transport", err: errors.New("attaching exec: connection reset by peer"), wantEnv: true},
		{name: "timed out and kill failed", res: contracts.ExecResult{TimedOut: true}, err: errBackendKilled, wantEnv: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail, err := classifyExec(context.Background(), "build command", tt.res, tt.err)
			if tt.wantEnv {
				if !errors.Is(err, ErrEnvironment) {
					t.Fatalf("classifyExec = %q, %v; want ErrEnvironment", detail, err)
				}
				if tt.err != nil && !errors.Is(err, tt.err) {
					t.Errorf("classifyExec dropped the cause: %v", err)
				}
				return
			}
			if err != nil || detail != tt.wantDetail {
				t.Errorf("classifyExec = %q, %v; want %q, nil", detail, err, tt.wantDetail)
			}
		})
	}
}

// TestClassifyExec_Cancelled: when the caller gave up, the result is the
// context's error, whatever the backend joined to it.
func TestClassifyExec_Cancelled(t *testing.T) {
	tests := []struct {
		name   string
		cancel func(context.Context) (context.Context, context.CancelFunc)
		want   error
	}{
		{name: "cancelled", cancel: context.WithCancel, want: context.Canceled},
		{name: "deadline", cancel: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, 0)
		}, want: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := tt.cancel(context.Background())
			cancel()
			execErr := errors.Join(ctx.Err(), sandbox.ErrOutputTooLarge, errBackendKilled)
			_, err := classifyExec(ctx, "tests command", contracts.ExecResult{}, execErr)
			if !errors.Is(err, tt.want) || errors.Is(err, ErrEnvironment) {
				t.Fatalf("classifyExec error = %v, want %v and not ErrEnvironment", err, tt.want)
			}
			err = stepError(ctx, "wiping", execErr)
			if !errors.Is(err, tt.want) || errors.Is(err, ErrEnvironment) {
				t.Fatalf("stepError = %v, want %v and not ErrEnvironment", err, tt.want)
			}
		})
	}
}

// TestStepError: a platform step's Exec error is the environment's, even
// an output overflow, and keeps its cause.
func TestStepError(t *testing.T) {
	for _, cause := range []error{sandbox.ErrOutputTooLarge, errBackendKilled, errors.New("daemon gone")} {
		err := stepError(context.Background(), "wiping sandbox", cause)
		if !errors.Is(err, ErrEnvironment) || !errors.Is(err, cause) || !strings.Contains(err.Error(), "wiping sandbox") {
			t.Errorf("stepError(%v) = %v", cause, err)
		}
	}
}
