package activity

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/activity"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/gate"
)

// Deliver is the ticket 8 stub of delivery. Its signature is ticket 9's,
// so ticket 9 fills the body without changing the call: it takes the
// attempt, the artifact hash and the base commit it would push.
//
// The stub delivers nothing. It checks what it would push: the attempt
// must have passed, the hash must be the one the attempt recorded, the
// base must be the commit its plan pinned, and gate.Verify, the single
// check before a push, must accept the artifact. It then logs the hash
// and base and reports Delivered false with DetailDeliveryStub; the
// workflow moves the feature request to needs_attention with that
// detail, so no run before ticket 9 can end delivered.
//
// Idempotent retry: it only reads. There is no outside action yet;
// ticket 9's push and pull request are idempotent by branch lease and by
// returning the existing pull request.
func (a *Activities) Deliver(ctx context.Context, attemptID, artifactHash, baseSHA string) (DeliverResult, error) {
	res, err := a.deliver(ctx, attemptID, artifactHash, baseSHA)
	return res, Temporal(err)
}

func (a *Activities) deliver(ctx context.Context, attemptID, artifactHash, baseSHA string) (DeliverResult, error) {
	row, err := readAttempt(ctx, a.d.DB, attemptID)
	if err != nil {
		return DeliverResult{}, err
	}
	pc, err := readPlanContext(ctx, a.d.DB, row.TaskID)
	if err != nil {
		return DeliverResult{}, err
	}
	switch {
	case row.State != contracts.AttemptPassed:
		return DeliverResult{}, fmt.Errorf("%w: attempt %s is %s, not passed", errNonRetryable, attemptID, row.State)
	case row.ArtifactHash != artifactHash:
		return DeliverResult{}, fmt.Errorf("%w: attempt %s recorded artifact %s, not %s", errNonRetryable, attemptID, row.ArtifactHash, artifactHash)
	case pc.BaseSHA != baseSHA:
		return DeliverResult{}, fmt.Errorf("%w: plan %s pinned %s, not %s", errNonRetryable, pc.PlanID, pc.BaseSHA, baseSHA)
	}
	if _, err := gate.Verify(ctx, a.d.Blobs, a.gateResults, attemptID, pc.RepoID, artifactHash); err != nil {
		return DeliverResult{}, err
	}
	activity.GetLogger(ctx).Info("delivery stub: artifact verified, not pushed until ticket 9",
		"attempt_id", attemptID, "artifact_hash", artifactHash, "base_sha", baseSHA)
	return DeliverResult{ArtifactHash: artifactHash, BaseSHA: baseSHA, Detail: DetailDeliveryStub}, nil
}
