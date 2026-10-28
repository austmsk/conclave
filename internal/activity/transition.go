package activity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/store"
)

// Transition makes one compare-and-set state change of a feature request
// or a task and returns the record's new revision.
//
// Idempotent retry: the change is conditional on the state and revision
// the workflow last saw (RL-13). A retry after a commit whose result was
// lost meets store.ErrAlreadyApplied, which is success here and returns
// the same revision the first call made. A change somebody else made
// first is a WrongStateError, which no retry can fix, so it is returned as
// permanent. There is no outside action.
func (a *Activities) Transition(ctx context.Context, in TransitionInput) (int, error) {
	rev, err := a.transition(ctx, in)
	return rev, Temporal(err)
}

func (a *Activities) transition(ctx context.Context, in TransitionInput) (int, error) {
	if in.Kind != contracts.EntityFeatureRequest && in.Kind != contracts.EntityTask {
		return 0, fmt.Errorf("%w: %s changes state through its own activity", errNonRetryable, in.Kind)
	}
	req := store.TransitionRequest{
		Kind: in.Kind, ID: in.ID, From: in.From, To: in.To,
		Revision: in.Revision, Reason: in.Reason, Actor: in.Actor,
	}
	err := pgx.BeginFunc(ctx, a.d.DB, func(tx pgx.Tx) error {
		return applied(store.Transition(ctx, tx, req))
	})
	if err != nil {
		return 0, fmt.Errorf("moving %s %s from %s to %s: %w", in.Kind, in.ID, in.From, in.To, err)
	}
	return in.Revision + 1, nil
}

// applied treats a transition that already landed as success: the earlier
// call committed and its result was not observed.
func applied(err error) error {
	if errors.Is(err, store.ErrAlreadyApplied) {
		return nil
	}
	return err
}
