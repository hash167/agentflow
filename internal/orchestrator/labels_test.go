package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

func TestDefaultLabelRulesResolveModels(t *testing.T) {
	t.Setenv("CURSOR_REVIEW_MODEL", "")
	t.Setenv("CURSOR_REVIEW_MODEL_CLAUDE", "")
	cfg, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"]}}`))
	require.NoError(t, err)
	require.Equal(t, []LabelRule{
		{Label: LabelDefault, Key: "default", Model: "grok-4.7"},
		{Label: LabelClaude, Key: "claude", Model: DefaultClaudeModel},
	}, cfg.Review.Labels)
}

func TestLabelModelOverrides(t *testing.T) {
	t.Setenv("CURSOR_REVIEW_MODEL", "grok-4.7-high")
	t.Setenv("CURSOR_REVIEW_MODEL_CLAUDE", "claude-opus-5-5-max")
	cfg, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"]}}`))
	require.NoError(t, err)
	require.Equal(t, "grok-4.7-high", cfg.Review.Labels[0].Model)
	require.Equal(t, "claude-opus-5-5-max", cfg.Review.Labels[1].Model)

	// review.defaultModel beats the env default; explicit rule model is kept
	// unless the per-key env override is set.
	t.Setenv("CURSOR_REVIEW_MODEL_CLAUDE", "")
	cfg, err = LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"],"defaultModel":"m-default",
		"labels":[{"label":"agentic-review-default","key":"default"},{"label":"agentic-review-claude","key":"claude","model":"m-claude"}]}}`))
	require.NoError(t, err)
	require.Equal(t, "m-default", cfg.Review.Labels[0].Model)
	require.Equal(t, "m-claude", cfg.Review.Labels[1].Model)
}

func TestLabelRulesRejectInvalid(t *testing.T) {
	for _, labels := range []string{
		`[{"label":"","key":"a"}]`,
		`[{"label":"x","key":"Bad Key"}]`,
		`[{"label":"x","key":"a"},{"label":"y","key":"a"}]`,
		`[{"label":"x","key":"a"},{"label":"X","key":"b"}]`,
	} {
		_, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"],"labels":`+labels+`}}`))
		require.Error(t, err, labels)
	}
}

func TestMarkersDoNotCollide(t *testing.T) {
	require.Equal(t, ReviewMarker, MarkerFor("default"))
	require.Equal(t, ReviewMarker, MarkerFor(""))
	claude := formatComment(ReviewResult{HeadSHA: "abcdef0123", Summary: "s", LabelKey: "claude", Label: LabelClaude, Model: "claude-opus-5-5-high"})
	require.Contains(t, claude, MarkerFor("claude"))
	require.NotContains(t, claude, ReviewMarker)
	require.Contains(t, claude, "`claude-opus-5-5-high`")
	require.Equal(t, "claude-opus-5-5-high", ModelFromComment(claude))
	def := formatComment(ReviewResult{HeadSHA: "abcdef0123", Summary: "s", LabelKey: "default", Label: LabelDefault, Model: "grok-4.7"})
	require.Contains(t, def, ReviewMarker)
	require.NotContains(t, def, MarkerFor("claude"))
	require.Contains(t, def, "requested by label `agentic-review-default`")
	require.Equal(t, "grok-4.7", ModelFromComment(def))
}

func TestPostReviewOneCommentPerLabel(t *testing.T) {
	f := newFakeGitHub(t)
	a := githubActivities(t, f)
	ctx := context.Background()
	// A pre-label AgentFlow comment exists; the default label reuses it.
	f.comments = append(f.comments, issueComment{ID: 50, Body: ReviewMarker + "\n<!-- agentflow-head: 1111111 -->\nold"})

	def := ReviewResult{Repo: "o/r", PRNumber: 8, HeadSHA: "aaaaaaa1", Summary: "d", LabelKey: "default", Label: LabelDefault, Model: "grok-4.7"}
	cl := ReviewResult{Repo: "o/r", PRNumber: 8, HeadSHA: "aaaaaaa1", Summary: "c", LabelKey: "claude", Label: LabelClaude, Model: "claude-opus-5-5-high"}
	require.NoError(t, a.postToGitHub(ctx, def))
	require.NoError(t, a.postToGitHub(ctx, cl))
	require.NoError(t, a.postToGitHub(ctx, cl)) // retry updates in place
	require.Equal(t, 1, f.posts, "only the claude comment is new")
	require.Equal(t, 2, f.patches)
	var defBodies, clBodies int
	for _, c := range f.comments {
		if strings.Contains(c.Body, ReviewMarker) {
			defBodies++
			require.Equal(t, "grok-4.7", ModelFromComment(c.Body))
		}
		if strings.Contains(c.Body, MarkerFor("claude")) {
			clBodies++
			require.Equal(t, "claude-opus-5-5-high", ModelFromComment(c.Body))
		}
	}
	require.Equal(t, 1, defBodies)
	require.Equal(t, 1, clBodies)
}

func TestLastReviewedTargetsIgnoresPreLabelComments(t *testing.T) {
	f := newFakeGitHub(t)
	a := githubActivities(t, f)
	f.comments = append(f.comments, issueComment{ID: 50, Body: ReviewMarker + "\n<!-- agentflow-head: aaaaaaa1 -->\nold"})
	pr := PRInfo{Repo: "o/r", PRNumber: 8, HeadSHA: "aaaaaaa1"}
	targets := []ReviewTarget{
		{PRInfo: pr, LabelKey: "default", Label: LabelDefault, Model: "grok-4.7"},
		{PRInfo: pr, LabelKey: "claude", Label: LabelClaude, Model: "claude-opus-5-5-high"},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a)
	v, err := env.ExecuteActivity(a.LastReviewedTargets, targets)
	require.NoError(t, err)
	var last map[string]Reviewed
	require.NoError(t, v.Get(&last))
	require.Empty(t, last, "a comment without a model marker must not dedupe a newly labelled PR")
	require.Len(t, TargetsNeedingReview(targets, last), 2)

	// After both labelled reviews post, both dedupe.
	require.NoError(t, a.postToGitHub(context.Background(), ReviewResult{Repo: "o/r", PRNumber: 8, HeadSHA: "aaaaaaa1", LabelKey: "default", Model: "grok-4.7"}))
	require.NoError(t, a.postToGitHub(context.Background(), ReviewResult{Repo: "o/r", PRNumber: 8, HeadSHA: "aaaaaaa1", LabelKey: "claude", Model: "claude-opus-5-5-high"}))
	v, err = env.ExecuteActivity(a.LastReviewedTargets, targets)
	require.NoError(t, err)
	require.NoError(t, v.Get(&last))
	require.Empty(t, TargetsNeedingReview(targets, last))
}

func TestListOpenPRsReadsLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/repos":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"full_name": "o/r", "owner": map[string]string{"login": "o"}}})
		case "/repos/o/r/pulls":
			_, _ = w.Write([]byte(`[{"number":1,"head":{"sha":"a1"},"labels":[]},
				{"number":2,"head":{"sha":"b2"},"labels":[{"name":"agentic-review-claude"},{"name":"agentic-review-default"}]}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("GITHUB_TOKEN", "tok")
	a := &Activities{
		Review: ReviewConfig{Sink: SinkConfig{Type: "github", APIBase: srv.URL}, Labels: testRules},
		Poller: PollerConfig{Owner: "o", IncludeDrafts: true},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a)
	v, err := env.ExecuteActivity(a.ListOpenPRs)
	require.NoError(t, err)
	var res ListOpenPRsResult
	require.NoError(t, v.Get(&res))
	require.Len(t, res.PRs, 2)
	require.Len(t, res.Targets, 2)
	require.Equal(t, "review:o/r#2:default", res.Targets[0].Key())
	require.Equal(t, "review:o/r#2:claude", res.Targets[1].Key())
}

func TestRunReviewAgentPassesPerWorkflowModel(t *testing.T) {
	t.Setenv("CURSOR_REVIEW_MODEL", "grok-4.7")
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	// The script echoes the env model and the stdin model back as the summary.
	a := &Activities{Review: ReviewConfig{MaxDiffBytes: 1000, ReviewerCommand: []string{"sh", "-c",
		`in=$(cat); m=$(printf '%s' "$in" | sed -n 's/.*"model":"\([^"]*\)".*/\1/p'); printf '{"summary":"%s|%s","findings":[]}' "$CURSOR_REVIEW_MODEL" "$m"`}}}
	env.RegisterActivity(a)
	v, err := env.ExecuteActivity(a.RunReviewAgent, ReviewRequest{Repo: "o/r", PRNumber: 1, Diff: "+x", LabelKey: "claude", Label: LabelClaude, Model: "claude-opus-5-5-high"})
	require.NoError(t, err)
	var res ReviewResult
	require.NoError(t, v.Get(&res))
	require.Equal(t, "claude-opus-5-5-high|claude-opus-5-5-high", res.Summary)
	require.Equal(t, "claude", res.LabelKey)
	require.Equal(t, "claude-opus-5-5-high", res.Model)
}
