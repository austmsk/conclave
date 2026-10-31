package activity

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
)

// Error types carried by the ApplicationErrors activities return. Typed
// errors do not survive the activity boundary: the workflow receives a
// *temporal.ApplicationError, where errors.Is and errors.As no longer see
// the cause, so it switches on these strings instead.
const (
	// ErrTypePermanent marks an error a retry cannot fix. It is never
	// retried.
	ErrTypePermanent = "permanent"
	// ErrTypeRateLimited marks a rate-limit refusal. It is retried after
	// the delay the refusal named.
	ErrTypeRateLimited = "rate_limited"
	// errTypeBudgetCap prefixes a cap refusal's type: budget_cap:<kind>.
	errTypeBudgetCap = ReasonBudgetCap + ":"
)

// errNonRetryable marks this package's own refusals, which a retry would
// meet again: an input that does not match what is recorded, a missing
// configuration entry.
var errNonRetryable = errors.New("activity refused")

// BudgetCapType is the ApplicationError type of a refusal by the given
// cap.
func BudgetCapType(kind llm.BudgetKind) string {
	return errTypeBudgetCap + string(kind)
}

// BudgetKindOf returns the cap kind named by an ApplicationError type, and
// whether the type is a cap refusal at all.
func BudgetKindOf(errType string) (llm.BudgetKind, bool) {
	kind, ok := strings.CutPrefix(errType, errTypeBudgetCap)
	if !ok || kind == "" {
		return "", false
	}
	return llm.BudgetKind(kind), true
}

// Temporal maps an error for the activity boundary (decision 0012):
//   - a BudgetError becomes non-retryable with type budget_cap:<kind>,
//     which the workflow routes to needs_attention;
//   - a rate-limit refusal that says how long to wait becomes retryable
//     with that NextRetryDelay;
//   - an error IsPermanent recognises becomes non-retryable with type
//     permanent;
//   - a context error and an ApplicationError pass through unchanged, and
//     anything else stays retryable under the activity's retry policy.
//
// Only the error's text crosses: the messages of every package these
// activities call are written to be safe to persist.
func Temporal(err error) error {
	if err == nil {
		return nil
	}
	var app *temporal.ApplicationError
	if errors.As(err, &app) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var budget llm.BudgetError
	if errors.As(err, &budget) {
		return temporal.NewNonRetryableApplicationError(err.Error(), BudgetCapType(budget.Kind), nil)
	}
	if delay := retryAfter(err); delay > 0 {
		return temporal.NewApplicationErrorWithOptions(err.Error(), ErrTypeRateLimited,
			temporal.ApplicationErrorOptions{NextRetryDelay: delay})
	}
	if IsPermanent(err) {
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypePermanent, nil)
	}
	return err
}

// retryAfter returns the wait a rate-limit refusal named, or zero.
func retryAfter(err error) time.Duration {
	var rl *llm.RateLimitError
	if errors.As(err, &rl) {
		return rl.RetryAfter
	}
	var api *github.APIError
	if errors.As(err, &api) && errors.Is(err, github.ErrRateLimited) {
		return api.RetryAfter
	}
	return 0
}

// storePermanent are the store's refusals that a retry meets again: the
// caller has confused two runs, named a row that does not exist, or lost
// a race it must not retry blindly.
var storePermanent = []error{
	errNonRetryable,
	store.ErrWorkflowMismatch,
	store.ErrSandboxMismatch,
	store.ErrArtifactMismatch,
	store.ErrInvalidID,
	store.ErrNotFound,
	store.ErrApprovalNeedsBaseSHA,
	store.ErrAnotherPlanApproved,
	store.ErrInvalidCommandRecord,
	agent.ErrInvalidConfig,
	gate.ErrNotVerified,
}

// IsPermanent reports whether err is one no retry can fix: a permanent
// router or GitHub error, a store refusal, a lost compare-and-set, or this
// package's own refusal.
func IsPermanent(err error) bool {
	if llm.IsPermanent(err) || github.IsPermanent(err) {
		return true
	}
	for _, p := range storePermanent {
		if errors.Is(err, p) {
			return true
		}
	}
	var wrong *store.WrongStateError
	var illegal *store.IllegalTransitionError
	return errors.As(err, &wrong) || errors.As(err, &illegal)
}
