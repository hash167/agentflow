package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
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
