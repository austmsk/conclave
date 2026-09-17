package contracts

// TaskSpec is the unit of work handed to an implementer. In Milestone 1 a
// feature request produces exactly one task; later milestones produce a graph.
type TaskSpec struct {
	// Key is stable within a plan (for example "T2-endpoint"). It appears in
	// branch names, commit trailers, and evaluation data.
	Key string `json:"key"`

	Description string               `json:"description"`
	Criteria    []AcceptanceCriteria `json:"criteria"`

	// PredictedFiles is the planner's estimate of the files this task touches.
	// Used for footprint-based serialization and, once actual changes are
	// known, for measuring prediction accuracy. Empty in Milestone 1.
	PredictedFiles []string `json:"predicted_files,omitempty"`
}

// AcceptanceCriteria describes observable behaviour, not implementation. A
// criterion a test cannot check is a planning defect.
type AcceptanceCriteria struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Constraints bound what an attempt may produce. They come from repository
// configuration in the platform repository, never from the target repository,
// so an agent's pull request cannot widen its own limits.
type Constraints struct {
	// MaxChangedLines caps the diff. Exceeding it fails the gate.
	MaxChangedLines int `json:"max_changed_lines"`

	// ProtectedPaths require explicit owner approval to change (FR-SEC8).
	ProtectedPaths []string `json:"protected_paths"`

	// ExecutionSurfacePaths cause code to run at install, build, or commit
	// time, or reconfigure the owner's AI tooling. Changes here are flagged
	// and surfaced at the top of the pull request (FR-SEC25).
	ExecutionSurfacePaths []string `json:"execution_surface_paths"`

	// AllowDependencyAdditions gates the request_dependency tool.
	AllowDependencyAdditions bool `json:"allow_dependency_additions"`
}

// RepoProfile is the minimal repository understanding available in Milestone 1.
// Milestone 3 replaces it with a generated codebase brief.
type RepoProfile struct {
	FullName   string `json:"full_name"`
	Language   string `json:"language"`
	LayoutHint string `json:"layout_hint"`

	// Conventions is a short prose note on naming, error handling, and test
	// style, maintained by the owner until briefs exist.
	Conventions string `json:"conventions"`
}
