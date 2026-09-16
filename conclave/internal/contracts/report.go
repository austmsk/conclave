package contracts

// CompletionReport is the structured payload of a finish tool call. The
// substantive output of an attempt is the diff, which platform code extracts
// from the sandbox; this report is what the agent claims about that diff.
//
// Claims are cross-checked against reality. A FilesChanged list that disagrees
// with the actual diff flags the attempt rather than being trusted.
type CompletionReport struct {
	Summary string `json:"summary"`

	FilesChanged []string `json:"files_changed"`
	TestsAdded   []string `json:"tests_added"`

	// Decisions are recorded at the moment they are made, so /ask answers can
	// cite real reasoning instead of reconstructing a plausible rationale
	// after the fact (FR-A6).
	Decisions []Decision `json:"decisions"`

	// Uncertainties surface in the pull request's risk section.
	Uncertainties []string `json:"uncertainties"`

	// DependenciesRequested is cross-checked against approved requests.
	DependenciesRequested []string `json:"dependencies_requested,omitempty"`
}

// Decision records one non-obvious choice.
type Decision struct {
	Subject           string   `json:"subject"`
	OptionsConsidered []string `json:"options_considered"`
	Chosen            string   `json:"chosen"`
	Rationale         string   `json:"rationale"`
}

// Provenance accompanies every agent output. Without exact model and prompt
// versions, evaluation results across runs are not comparable.
type Provenance struct {
	Role          Role   `json:"role"`
	PromptVersion string `json:"prompt_version"`
	Model         string `json:"model"`
	Provider      string `json:"provider"`
}

// Usage is the cost accounting for one session.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Steps        int     `json:"steps"`
}
