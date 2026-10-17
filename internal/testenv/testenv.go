package testenv

import (
	"os"
	"testing"
)

// RequireEnv names the variable that turns skips into failures. CI sets it
// through `make test-ci`.
const RequireEnv = "CONCLAVE_REQUIRE_INTEGRATION"

// Skip skips t with the given reason, or fails it when RequireEnv is set.
// Use it in place of t.Skip wherever a test needs part of the environment.
func Skip(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv(RequireEnv) != "" {
		t.Fatalf("required by "+RequireEnv+": "+format, args...)
		return
	}
	t.Skipf(format, args...)
}
