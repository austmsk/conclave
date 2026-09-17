package contracts

import "slices"

// FeatureRequestStatus is the six-status lifecycle of a feature request.
//
// There is no automatic failed status: every problem routes to
// FeatureNeedsAttention with a reason, and the owner decides what happens
// next. The workflow ends at FeatureDelivered; merge outcomes are recorded on
// pull request records via webhooks rather than by keeping the workflow open
// (RL-9).
type FeatureRequestStatus string

const (
	FeaturePlanning         FeatureRequestStatus = "planning"
	FeatureAwaitingApproval FeatureRequestStatus = "awaiting_approval"
	FeatureExecuting        FeatureRequestStatus = "executing"
	FeatureNeedsAttention   FeatureRequestStatus = "needs_attention"
	FeatureDelivered        FeatureRequestStatus = "delivered"
	FeatureCancelled        FeatureRequestStatus = "cancelled"
)

// PlanStatus is the lifecycle of one plan version. A feature request may have
// many versions but at most one approved plan at a time; the schema enforces
// that with a partial unique index (RL-4).
type PlanStatus string

const (
	PlanProposed   PlanStatus = "proposed"
	PlanApproved   PlanStatus = "approved"
	PlanSuperseded PlanStatus = "superseded"
	PlanRejected   PlanStatus = "rejected"
)

// TaskStatus is the Milestone 1 task lifecycle. Later milestones add statuses
// (for example integrated); they are added here and to the transition table,
// never inferred elsewhere.
type TaskStatus string

const (
	TaskRunning        TaskStatus = "running"
	TaskDone           TaskStatus = "done"
	TaskNeedsAttention TaskStatus = "needs_attention"
	TaskCancelled      TaskStatus = "cancelled"
)

// EntityKind names a record kind whose state is governed by the transition
// table. The values are stable: they appear in the state_transitions audit
// table.
type EntityKind string

const (
	EntityFeatureRequest EntityKind = "feature_request"
	EntityPlan           EntityKind = "plan"
	EntityTask           EntityKind = "task"
	EntityAttempt        EntityKind = "attempt"
)

// The transition table. Each map lists, for every state, the states a record
// may move to. A state that maps to an empty slice is terminal. Every state
// must appear as a key, so the exhaustive test can enumerate the state space
// from the table itself and compare it against the documented lifecycle.
//
// This is the single source of truth for legality (RL-13). The store's
// compare-and-set helper consults it before touching the database, and
// workflow code consults it when deciding whether a command applies, so
// neither side can disagree with the other.
var (
	featureTransitions = map[FeatureRequestStatus][]FeatureRequestStatus{
		FeaturePlanning:         {FeatureAwaitingApproval, FeatureNeedsAttention, FeatureCancelled},
		FeatureAwaitingApproval: {FeatureExecuting, FeaturePlanning, FeatureCancelled},
		FeatureExecuting:        {FeatureDelivered, FeatureNeedsAttention, FeatureCancelled},
		FeatureNeedsAttention:   {FeatureExecuting, FeaturePlanning, FeatureCancelled},
		FeatureDelivered:        {},
		FeatureCancelled:        {},
	}

	planTransitions = map[PlanStatus][]PlanStatus{
		PlanProposed:   {PlanApproved, PlanSuperseded, PlanRejected},
		PlanApproved:   {PlanSuperseded},
		PlanSuperseded: {},
		PlanRejected:   {},
	}

	taskTransitions = map[TaskStatus][]TaskStatus{
		TaskRunning:        {TaskDone, TaskNeedsAttention, TaskCancelled},
		TaskNeedsAttention: {TaskRunning, TaskCancelled},
		TaskDone:           {},
		TaskCancelled:      {},
	}

	attemptTransitions = map[AttemptState][]AttemptState{
		AttemptRunning: {AttemptPassed, AttemptFailed, AttemptErrored},
		AttemptPassed:  {},
		AttemptFailed:  {},
		AttemptErrored: {},
	}
)

// CanTransition reports whether a record of the given kind may move from one
// state to another. Unknown kinds, unknown states, and self-transitions are
// illegal: an already-applied transition is detected by the store helper, not
// by treating "from == to" as a legal move.
func CanTransition(kind EntityKind, from, to string) bool {
	switch kind {
	case EntityFeatureRequest:
		return FeatureRequestStatus(from).CanMoveTo(FeatureRequestStatus(to))
	case EntityPlan:
		return PlanStatus(from).CanMoveTo(PlanStatus(to))
	case EntityTask:
		return TaskStatus(from).CanMoveTo(TaskStatus(to))
	case EntityAttempt:
		return AttemptState(from).CanMoveTo(AttemptState(to))
	default:
		return false
	}
}

// States returns every state of the given kind in sorted order, or nil for an
// unknown kind. Sorted so callers can range over it deterministically.
func States(kind EntityKind) []string {
	switch kind {
	case EntityFeatureRequest:
		return sortedKeys(featureTransitions)
	case EntityPlan:
		return sortedKeys(planTransitions)
	case EntityTask:
		return sortedKeys(taskTransitions)
	case EntityAttempt:
		return sortedKeys(attemptTransitions)
	default:
		return nil
	}
}

// CanMoveTo reports whether the feature request may move to the given status.
func (s FeatureRequestStatus) CanMoveTo(to FeatureRequestStatus) bool {
	return slices.Contains(featureTransitions[s], to)
}

// CanMoveTo reports whether the plan may move to the given status.
func (s PlanStatus) CanMoveTo(to PlanStatus) bool {
	return slices.Contains(planTransitions[s], to)
}

// CanMoveTo reports whether the task may move to the given status.
func (s TaskStatus) CanMoveTo(to TaskStatus) bool {
	return slices.Contains(taskTransitions[s], to)
}

// CanMoveTo reports whether the attempt may move to the given state.
func (s AttemptState) CanMoveTo(to AttemptState) bool {
	return slices.Contains(attemptTransitions[s], to)
}

func sortedKeys[K ~string, V any](m map[K]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	slices.Sort(keys)
	return keys
}

// EntityKinds returns every kind the transition table governs, in sorted
// order. Tests range over it so a kind cannot be added without being
// exercised end to end.
func EntityKinds() []EntityKind {
	return []EntityKind{EntityAttempt, EntityFeatureRequest, EntityPlan, EntityTask}
}
