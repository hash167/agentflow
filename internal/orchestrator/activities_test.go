package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"agent-gateway/internal/telemetry"
)

func runReviewer(t *testing.T, script string) error {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	a := &Activities{Review: ReviewConfig{MaxDiffBytes: 1000, ReviewerCommand: []string{"sh", "-c", script}}}
	env.RegisterActivity(a)
	_, err := env.ExecuteActivity(a.RunReviewAgent, ReviewRequest{Repo: "o/r", PRNumber: 1, Diff: "+x"})
	return err
}

func nonRetryable(err error) bool {
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.NonRetryable()
}

func TestRunReviewAgentValidJSON(t *testing.T) {
	require.NoError(t, runReviewer(t, `cat >/dev/null; echo '{"summary":"ok","findings":[]}'`))
}

func TestRunReviewAgentInvalidJSONIsNonRetryable(t *testing.T) {
	err := runReviewer(t, `cat >/dev/null; echo 'not json'`)
	require.Error(t, err)
	require.True(t, nonRetryable(err), "%v", err)
}

func TestRunReviewAgentExit3IsNonRetryable(t *testing.T) {
	err := runReviewer(t, `cat >/dev/null; echo 'bad verdict' >&2; exit 3`)
	require.True(t, nonRetryable(err), "%v", err)
	require.Contains(t, err.Error(), "bad verdict")
}

func TestRunReviewAgentExit1IsRetryable(t *testing.T) {
	err := runReviewer(t, `cat >/dev/null; exit 1`)
	require.Error(t, err)
	require.False(t, nonRetryable(err))
}

func TestFormatCommentHasMarkerAndSHA(t *testing.T) {
	body := formatComment(ReviewResult{HeadSHA: "0123456789abcdef", Summary: "s"})
	require.Contains(t, body, ReviewMarker)
	require.Equal(t, "0123456789abcdef", HeadSHAFromComment(body))
	_ = context.Background()
}

func TestRunReviewAgentPassesConfiguredReviewerTimeout(t *testing.T) {
	t.Setenv("CURSOR_REVIEW_TIMEOUT", "8m")
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	a := &Activities{Review: ReviewConfig{MaxDiffBytes: 1000, ReviewerTimeout: "12m0s", ReviewerCommand: []string{"sh", "-c",
		`cat >/dev/null; printf '{"summary":"%s","findings":[]}' "$CURSOR_REVIEW_TIMEOUT"`}}}
	env.RegisterActivity(a)
	future, err := env.ExecuteActivity(a.RunReviewAgent, ReviewRequest{Repo: "o/r", PRNumber: 1, Diff: "+x"})
	require.NoError(t, err)
	var result ReviewResult
	require.NoError(t, future.Get(&result))
	require.Equal(t, "12m0s", result.Summary)
}

func TestRunReviewAgentSuccessEmitsSpans(t *testing.T) {
	sr, cleanup := telemetry.SetupTest()
	defer cleanup()
	require.NoError(t, runReviewer(t, `cat >/dev/null; echo '{"summary":"ok","findings":[]}'`))
	names := telemetry.EndedNames(sr)
	require.Contains(t, names, telemetry.SpanAttemptStarted)
	require.Contains(t, names, telemetry.SpanAttemptFinished)
	require.Contains(t, names, telemetry.SpanInvokeAgent)
	require.Contains(t, names, telemetry.SpanReviewerSubproc)
	require.Contains(t, names, telemetry.SpanReviewerParse)
	attrs := telemetry.AttrsOf(sr, telemetry.SpanInvokeAgent)
	require.Equal(t, "o/r", attrs[telemetry.AttrRepo])
	require.Equal(t, "1", attrs[telemetry.AttrPRNumber])
	require.Equal(t, telemetry.OutcomeReviewedClean, attrs[telemetry.AttrOutcome])
}

func TestRunReviewAgentErrorClassAndPrivacy(t *testing.T) {
	sr, cleanup := telemetry.SetupTest()
	defer cleanup()
	t.Setenv("GITHUB_TOKEN", "ghp_SYNTHETICTESTTOKEN_DO_NOT_USE_ABCD")
	err := runReviewer(t, `cat >/dev/null; echo 'usage limit reached for opus ghp_SYNTHETICTESTTOKEN_DO_NOT_USE_ABCD' >&2; exit 1`)
	require.Error(t, err)
	dump := telemetry.DumpAttrs(sr)
	require.NotContains(t, dump, "ghp_SYNTHETICTESTTOKEN_DO_NOT_USE_ABCD")
	require.NotContains(t, dump, "+x")
	finished := telemetry.AttrsOf(sr, telemetry.SpanAttemptFinished)
	require.Equal(t, telemetry.ClassUsageLimit, finished[telemetry.AttrErrorClass])
	require.Equal(t, telemetry.OutcomeUsageLimited, finished[telemetry.AttrOutcome])
}

func TestDisabledTracingDoesNotBreakReview(t *testing.T) {
	// Default handle is a no-op (no SetupTest).
	require.NoError(t, runReviewer(t, `cat >/dev/null; echo '{"summary":"ok","findings":[]}'`))
}

func TestBrokenExporterDoesNotBreakReview(t *testing.T) {
	defer telemetry.SetupBroken()()
	require.NoError(t, runReviewer(t, `cat >/dev/null; echo '{"summary":"ok","findings":[]}'`))
}

func TestFetchDiffSpanOmitsBody(t *testing.T) {
	sr, cleanup := telemetry.SetupTest()
	defer cleanup()
	diff := "+++ b/secret.go\n+API_KEY=super-secret-diff-body\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(diff))
	}))
	defer srv.Close()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	a := &Activities{Review: ReviewConfig{MaxDiffBytes: 10000}}
	env.RegisterActivity(a)
	future, err := env.ExecuteActivity(a.FetchDiff, ReviewRequest{Repo: "o/r", PRNumber: 9, DiffURL: srv.URL, HeadSHA: "abc"})
	require.NoError(t, err)
	var got string
	require.NoError(t, future.Get(&got))
	require.Equal(t, diff, got)
	dump := telemetry.DumpAttrs(sr)
	require.NotContains(t, dump, "super-secret-diff-body")
	require.NotContains(t, dump, "API_KEY=")
	attrs := telemetry.AttrsOf(sr, telemetry.SpanDiffFetch)
	require.Equal(t, "200", attrs[telemetry.AttrHTTPStatus])
	require.NotEmpty(t, attrs[telemetry.AttrDiffSHA256])
}

func TestPostReviewFileSinkOutcome(t *testing.T) {
	sr, cleanup := telemetry.SetupTest()
	defer cleanup()
	dir := t.TempDir()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	a := &Activities{Review: ReviewConfig{Sink: SinkConfig{Type: "file", Dir: dir}}}
	env.RegisterActivity(a)
	res := ReviewResult{Repo: "o/r", PRNumber: 1, Summary: "s", Findings: []ReviewFinding{{File: "a.go", Severity: "error", Message: "do not put me in a span"}}}
	_, err := env.ExecuteActivity(a.PostReview, res)
	require.NoError(t, err)
	dump := telemetry.DumpAttrs(sr)
	require.NotContains(t, dump, "do not put me in a span")
	require.Equal(t, telemetry.OutcomeReviewed, telemetry.AttrsOf(sr, telemetry.SpanReviewOutcome)[telemetry.AttrOutcome])
}
