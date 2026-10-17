package testenv_test

import (
	"fmt"
	"testing"

	"github.com/austmsk/conclave/internal/testenv"
)

// recorder captures Skipf and Fatalf instead of stopping the test, so the
// test can assert which one Skip chose.
type recorder struct {
	testing.TB
	skipped, failed string
}

func (r *recorder) Helper() {}

func (r *recorder) Skipf(format string, args ...any) { r.skipped = fmt.Sprintf(format, args...) }

func (r *recorder) Fatalf(format string, args ...any) { r.failed = fmt.Sprintf(format, args...) }

func TestSkip(t *testing.T) {
	tests := []struct {
		name        string
		require     string
		wantSkipped string
		wantFailed  string
	}{
		{name: "unset skips", require: "", wantSkipped: "no docker: down"},
		{name: "set fails", require: "1", wantFailed: "required by " + testenv.RequireEnv + ": no docker: down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(testenv.RequireEnv, tt.require)
			r := &recorder{TB: t}
			testenv.Skip(r, "no docker: %s", "down")
			if r.skipped != tt.wantSkipped || r.failed != tt.wantFailed {
				t.Errorf("skipped %q, failed %q; want skipped %q, failed %q", r.skipped, r.failed, tt.wantSkipped, tt.wantFailed)
			}
		})
	}
}
