package llm

import "errors"

// permanent lists the errors a retry cannot fix: the same request against
// the same configuration fails the same way, and a retried model call costs
// money. ErrBudgetExceeded is the important one. FR-SEC9 says a cap pauses
// work, and an activity that keeps retrying into an exhausted budget is a
// retry storm, not a pause.
var permanent = []error{
	ErrBudgetExceeded,
	ErrInvalidRequest,
	ErrInvalidScope,
	ErrRepoMismatch,
	ErrNoRoute,
	ErrMissingProvider,
	ErrNoEmbeddingModel,
	ErrUnknownRepo,
	ErrNoTier,
	ErrUnknownTier,
	ErrNoRoleAllowlist,
	ErrUnknownProvider,
	ErrRoleDenied,
	ErrTierDenied,
	ErrTierMismatch,
	ErrAuth,
	ErrUnsupportedReply,
}

// IsPermanent reports whether err is one a retry cannot fix. The activity
// that calls the router must return such an error as non-retryable
// (temporal.NewNonRetryableApplicationError), and the workflow must treat
// ErrBudgetExceeded as a pause. This package does not import Temporal, so
// the wrapping lives with the activity.
func IsPermanent(err error) bool {
	for _, p := range permanent {
		if errors.Is(err, p) {
			return true
		}
	}
	return false
}
