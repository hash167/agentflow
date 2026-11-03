// Package orchestrator implements the code-review control plane: a Temporal
// workflow that turns PR events into agent review runs with retries,
// re-review on new pushes, and auditable results.
package orchestrator

// ReviewRequest is the input to one review cycle. Either Diff is provided
// inline or DiffURL points at a unified diff to fetch.
type ReviewRequest struct {
	Repo     string `json:"repo"` // e.g. "alice/widgets"
	PRNumber int    `json:"prNumber"`
	Title    string `json:"title,omitempty"`
	HeadSHA  string `json:"headSha,omitempty"`
	Diff     string `json:"diff,omitempty"`
	DiffURL  string `json:"diffUrl,omitempty"`
	// LabelKey/Label/Model come from the matching LabelRule. Model is passed
	// to the reviewer per workflow (stdin "model" and CURSOR_REVIEW_MODEL).
	LabelKey string `json:"labelKey,omitempty"`
	Label    string `json:"label,omitempty"`
	Model    string `json:"model,omitempty"`
}

type ReviewFinding struct {
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity"` // "info", "warning", "error"
	Message  string `json:"message"`
}

type ReviewResult struct {
	Repo     string          `json:"repo"`
	PRNumber int             `json:"prNumber"`
	HeadSHA  string          `json:"headSha,omitempty"`
	Summary  string          `json:"summary"`
	Findings []ReviewFinding `json:"findings"`
	// Skipped explains why the review was not run (e.g. diff too large).
	Skipped  string `json:"skipped,omitempty"`
	LabelKey string `json:"labelKey,omitempty"`
	Label    string `json:"label,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Status is returned by the workflow's "status" query.
type Status struct {
	Phase string `json:"phase"` // "fetching", "reviewing", "posting", "done"
	// HeadSHA is the head commit of the cycle in flight (or last finished).
	HeadSHA    string        `json:"headSha,omitempty"`
	Model      string        `json:"model,omitempty"`
	LabelKey   string        `json:"labelKey,omitempty"`
	Reviews    int           `json:"reviews"`
	LastResult *ReviewResult `json:"lastResult,omitempty"`
}

// SignalNewPush delivers an updated ReviewRequest to a running workflow.
const SignalNewPush = "new-push"

// QueryStatus returns the workflow's current Status.
const QueryStatus = "status"
