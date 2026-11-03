package orchestrator

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// LabelRule opts a PR into review: PRs carrying Label are reviewed with
// Model. Key names the review in workflow IDs ("review:<repo>#<n>:<key>"),
// comment markers and file names, so two labels on one PR never coalesce.
type LabelRule struct {
	Label string `json:"label"`
	Key   string `json:"key"`
	// Model is the Cursor CLI model id. Empty means the default model:
	// review.defaultModel, else $CURSOR_REVIEW_MODEL, else grok-4.7.
	// $CURSOR_REVIEW_MODEL_<KEY> (e.g. CURSOR_REVIEW_MODEL_CLAUDE) overrides it.
	Model string `json:"model,omitempty"`
}

const (
	LabelDefault = "agentic-review-default"
	LabelClaude  = "agentic-review-claude"
	// DefaultReviewModel matches the cursor-reviewer default.
	DefaultReviewModel = "grok-4.7"
	// DefaultClaudeModel is the most capable Claude in Cursor CLI
	// 2026.10.22 that is ZDR-compatible: Opus 5.5 at the high effort Cursor
	// recommends (adaptive thinking is always on for Opus 5.5).
	DefaultClaudeModel = "claude-opus-5-5-high"
)

// DefaultLabelRules is used when review.labels is not set.
func DefaultLabelRules() []LabelRule {
	return []LabelRule{
		{Label: LabelDefault, Key: "default"},
		{Label: LabelClaude, Key: "claude", Model: DefaultClaudeModel},
	}
}

var labelKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// resolveLabelRules validates rules and fills in models (env overrides,
// then the default model) so every rule has a concrete model id.
func resolveLabelRules(rules []LabelRule, defaultModel string) ([]LabelRule, error) {
	if defaultModel == "" {
		defaultModel = os.Getenv("CURSOR_REVIEW_MODEL")
	}
	if defaultModel == "" {
		defaultModel = DefaultReviewModel
	}
	seenKey, seenLabel := map[string]bool{}, map[string]bool{}
	out := make([]LabelRule, 0, len(rules))
	for _, r := range rules {
		r.Label = strings.TrimSpace(r.Label)
		r.Key = strings.ToLower(strings.TrimSpace(r.Key))
		if r.Label == "" || !labelKeyRe.MatchString(r.Key) {
			return nil, fmt.Errorf("review.labels: each rule needs a label and a key matching %s, got %+v", labelKeyRe, r)
		}
		if seenKey[r.Key] || seenLabel[strings.ToLower(r.Label)] {
			return nil, fmt.Errorf("review.labels: duplicate label or key in %+v", r)
		}
		seenKey[r.Key], seenLabel[strings.ToLower(r.Label)] = true, true
		envName := "CURSOR_REVIEW_MODEL_" + strings.ToUpper(strings.ReplaceAll(r.Key, "-", "_"))
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
			r.Model = v
		}
		if r.Model == "" {
			r.Model = defaultModel
		}
		out = append(out, r)
	}
	return out, nil
}

// MatchLabels returns the rules whose label is on the PR (case-insensitive,
// GitHub label names are case-insensitive), in rule order.
func MatchLabels(rules []LabelRule, labels []string) []LabelRule {
	var out []LabelRule
	for _, r := range rules {
		for _, l := range labels {
			if strings.EqualFold(strings.TrimSpace(l), r.Label) {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// ReviewWorkflowID is the per-PR, per-label workflow ID.
func ReviewWorkflowID(repo string, prNumber int, key string) string {
	return "review:" + repo + "#" + strconv.Itoa(prNumber) + ":" + key
}

// MarkerFor is the comment marker for a label key. The default label keeps
// the original marker so a PR's existing AgentFlow comment is updated in
// place once it is labelled; other keys get their own comment.
func MarkerFor(key string) string {
	if key == "" || key == "default" {
		return ReviewMarker
	}
	return "<!-- agentflow-review:" + key + " -->"
}

var modelMarkerRe = regexp.MustCompile(`<!-- agentflow-model: ([^ ]+) -->`)

// ModelFromComment extracts the model recorded in a marker comment.
func ModelFromComment(body string) string {
	if m := modelMarkerRe.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}
