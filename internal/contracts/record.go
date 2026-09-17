package contracts

import "time"

// Repo is a repository the platform may run on. A row exists only for
// repositories present in the platform's configuration, and Tier is required
// because a repository without a tier cannot start a run (FR-SEC12).
type Repo struct {
	ID            string          `json:"id"`
	FullName      string          `json:"full_name"`
	Tier          SensitivityTier `json:"tier"`
	DefaultBranch string          `json:"default_branch"`
}

// FeatureRequest is the durable record of one labeled issue. WorkflowID is
// recorded so every run is traceable from the database to the Temporal UI
// and back (RL-14).
type FeatureRequest struct {
	ID          string               `json:"id"`
	RepoID      string               `json:"repo_id"`
	IssueNumber int                  `json:"issue_number"`
	Title       string               `json:"title"`
	Status      FeatureRequestStatus `json:"status"`
	Reason      string               `json:"reason,omitempty"`
	WorkflowID  string               `json:"workflow_id"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

// Plan is one version of the plan for a feature request. BaseSHA is pinned
// at approval and all tasks run against it (FR-S19); it is empty until then.
type Plan struct {
	ID               string     `json:"id"`
	FeatureRequestID string     `json:"feature_request_id"`
	Version          int        `json:"version"`
	Status           PlanStatus `json:"status"`
	BaseSHA          string     `json:"base_sha,omitempty"`
	Tasks            []TaskSpec `json:"tasks"`
	CreatedAt        time.Time  `json:"created_at"`
	ApprovedAt       time.Time  `json:"approved_at,omitempty"`
}

// Task is the durable record of one unit of work from an approved plan.
type Task struct {
	ID     string     `json:"id"`
	PlanID string     `json:"plan_id"`
	Spec   TaskSpec   `json:"spec"`
	Status TaskStatus `json:"status"`
	Reason string     `json:"reason,omitempty"`
}

// CostEvent records one billable model call. IdempotencyKey is unique in the
// database: a retried activity that already recorded its cost inserts the same
// key and is ignored, which is how a cost event avoids being counted twice
// (RL-2).
type CostEvent struct {
	ID               string    `json:"id"`
	IdempotencyKey   string    `json:"idempotency_key"`
	RepoID           string    `json:"repo_id"`
	FeatureRequestID string    `json:"feature_request_id,omitempty"`
	AttemptID        string    `json:"attempt_id,omitempty"`
	Role             Role      `json:"role"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	InputTokens      int       `json:"input_tokens"`
	OutputTokens     int       `json:"output_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	OccurredAt       time.Time `json:"occurred_at"`
}

// StateTransition is one audit row: who moved which record from what to what,
// and when (FR-SEC10). Rows are append-only; the application database role
// cannot update or delete them.
type StateTransition struct {
	ID         string     `json:"id"`
	Kind       EntityKind `json:"entity_kind"`
	EntityID   string     `json:"entity_id"`
	From       string     `json:"from_state"`
	To         string     `json:"to_state"`
	Actor      string     `json:"actor"`
	Reason     string     `json:"reason,omitempty"`
	OccurredAt time.Time  `json:"occurred_at"`
}
