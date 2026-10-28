package activity

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
)

// TestTemporal maps each kind of error for the activity boundary: caps
// to budget_cap:<kind>, permanent errors to non-retryable, rate limits to
// retryable with the delay they named, and the rest unchanged.
func TestTemporal(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantType     string
		nonRetryable bool
		wantDelay    time.Duration
		passThrough  bool
	}{
		{name: "feature request cap", err: fmt.Errorf("calling: %w", llm.BudgetError{Kind: llm.BudgetFeatureRequest}), wantType: "budget_cap:feature_request", nonRetryable: true},
		{name: "monthly cap", err: llm.BudgetError{Kind: llm.BudgetMonth}, wantType: "budget_cap:month", nonRetryable: true},
		{name: "permanent router error", err: fmt.Errorf("x: %w", llm.ErrRoleDenied), wantType: ErrTypePermanent, nonRetryable: true},
		{name: "budget sentinel without a kind", err: llm.ErrBudgetExceeded, wantType: ErrTypePermanent, nonRetryable: true},
		{name: "permanent GitHub error", err: fmt.Errorf("x: %w", github.ErrNotFound), wantType: ErrTypePermanent, nonRetryable: true},
		{name: "lost compare-and-set", err: &store.WrongStateError{}, wantType: ErrTypePermanent, nonRetryable: true},
		{name: "store mismatch", err: store.ErrSandboxMismatch, wantType: ErrTypePermanent, nonRetryable: true},
		{name: "own refusal", err: fmt.Errorf("%w: no repo", errNonRetryable), wantType: ErrTypePermanent, nonRetryable: true},
		{name: "invalid agent config", err: agent.ErrInvalidConfig, wantType: ErrTypePermanent, nonRetryable: true},
		{name: "router rate limit", err: fmt.Errorf("x: %w", &llm.RateLimitError{RetryAfter: 42 * time.Second}), wantType: ErrTypeRateLimited, wantDelay: 42 * time.Second},
		{name: "rate limit with no delay is left to the policy", err: &llm.RateLimitError{}, passThrough: true},
		{name: "ordinary error", err: errors.New("connection reset"), passThrough: true},
		{name: "cancellation", err: context.Canceled, passThrough: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Temporal(tt.err)
			if tt.passThrough {
				if got != tt.err {
					t.Fatalf("Temporal(%v) = %v, want it unchanged", tt.err, got)
				}
				return
			}
			var app *temporal.ApplicationError
			if !errors.As(got, &app) {
				t.Fatalf("Temporal(%v) = %T, want an ApplicationError", tt.err, got)
			}
			if app.Type() != tt.wantType || app.NonRetryable() != tt.nonRetryable || app.NextRetryDelay() != tt.wantDelay {
				t.Errorf("type %q non-retryable %v delay %v; want %q %v %v",
					app.Type(), app.NonRetryable(), app.NextRetryDelay(), tt.wantType, tt.nonRetryable, tt.wantDelay)
			}
		})
	}
	if Temporal(nil) != nil {
		t.Error("Temporal(nil) is not nil")
	}
}

// TestBudgetKindOf reads the cap kind back from the type string, and
// refuses every other type.
func TestBudgetKindOf(t *testing.T) {
	tests := []struct {
		typ    string
		want   llm.BudgetKind
		wantOK bool
	}{
		{BudgetCapType(llm.BudgetFeatureRequest), llm.BudgetFeatureRequest, true},
		{BudgetCapType(llm.BudgetMonth), llm.BudgetMonth, true},
		{"budget_cap:", "", false},
		{ErrTypePermanent, "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := BudgetKindOf(tt.typ)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("BudgetKindOf(%q) = %q, %v; want %q, %v", tt.typ, got, ok, tt.want, tt.wantOK)
		}
	}
}

// TestAttemptTimeout is the decision 0012 sum for the shipped role and a
// five-minute command timeout: 3 + 20 + 15 + 2 + 5 = 45 minutes, above
// the router's ten-minute call bound.
func TestAttemptTimeout(t *testing.T) {
	got := AttemptTimeout(agent.Implementer, 5*time.Minute)
	if got != 45*time.Minute {
		t.Errorf("AttemptTimeout = %v, want 45m", got)
	}
	if got <= 10*time.Minute {
		t.Errorf("AttemptTimeout %v is not above the adapter timeout", got)
	}
}
