package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
)

func TestCodeReviewWorkflowCompletes(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	activities := &Activities{}
	env.RegisterActivity(activities)

	request := ReviewRequest{Repo: "alice/widgets", PRNumber: 7, Diff: "+ fmt.Println(\"x\")"}
	want := ReviewResult{Repo: "alice/widgets", PRNumber: 7, Summary: "looks fine", Findings: []ReviewFinding{}}

	env.OnActivity(activities.RunReviewAgent, mock.Anything, request).Return(want, nil).Once()
	env.OnActivity(activities.PostReview, mock.Anything, want).Return(nil).Once()

	env.ExecuteWorkflow(CodeReviewWorkflow, request)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result ReviewResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, want, result)

	response, err := env.QueryWorkflow(QueryStatus)
	require.NoError(t, err)
	var status Status
	require.NoError(t, response.Get(&status))
	require.Equal(t, "done", status.Phase)
	require.Equal(t, 1, status.Reviews)
	env.AssertExpectations(t)
}

func TestCodeReviewWorkflowFetchesDiffFromURL(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	activities := &Activities{}
	env.RegisterActivity(activities)

	request := ReviewRequest{Repo: "alice/widgets", PRNumber: 8, DiffURL: "http://example.test/pr8.diff"}
	withDiff := request
	withDiff.Diff = "+ added line"

	env.OnActivity(activities.FetchDiff, mock.Anything, request).Return("+ added line", nil).Once()
	env.OnActivity(activities.RunReviewAgent, mock.Anything, withDiff).
		Return(ReviewResult{Repo: request.Repo, PRNumber: request.PRNumber, Summary: "ok"}, nil).Once()
	env.OnActivity(activities.PostReview, mock.Anything, mock.Anything).Return(nil).Once()

	env.ExecuteWorkflow(CodeReviewWorkflow, request)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestOversizedDiffIsSkippedWithoutRunningReviewer(t *testing.T) {
	// Uses the real activity: the budget check happens before any command runs,
	// so no reviewer command is needed.
	activities := &Activities{Review: ReviewConfig{
		MaxDiffBytes:    10,
		ReviewerCommand: []string{"/nonexistent"},
		Sink:            SinkConfig{Type: "file", Dir: t.TempDir()},
	}}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities)

	future, err := env.ExecuteActivity(activities.RunReviewAgent, ReviewRequest{
		Repo: "alice/widgets", PRNumber: 9, Diff: "this diff is definitely longer than ten bytes",
	})
	require.NoError(t, err)

	var result ReviewResult
	require.NoError(t, future.Get(&result))
	require.Contains(t, result.Skipped, "over the 10 byte budget")
	require.NotContains(t, result.Skipped, "truncated")
	require.Empty(t, result.Findings)
}

func TestRunReviewAgentUsesConfiguredStartToClose(t *testing.T) {
	prev := ReviewActivityTimeout()
	t.Cleanup(func() { SetReviewActivityTimeout(prev) })
	SetReviewActivityTimeout(17 * time.Minute)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	activities := &Activities{}
	env.RegisterActivity(activities)

	request := ReviewRequest{Repo: "alice/widgets", PRNumber: 10, Diff: "+x"}
	got := map[string]time.Duration{}
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		got[info.ActivityType.Name] = info.Deadline.Sub(info.StartedTime)
	})
	env.OnActivity(activities.RunReviewAgent, mock.Anything, request).
		Return(ReviewResult{Repo: request.Repo, PRNumber: request.PRNumber, Summary: "ok"}, nil).Once()
	env.OnActivity(activities.PostReview, mock.Anything, mock.Anything).Return(nil).Once()

	env.ExecuteWorkflow(CodeReviewWorkflow, request)
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 17*time.Minute, got["RunReviewAgent"].Round(time.Second))
	require.Equal(t, 10*time.Minute, got["PostReview"].Round(time.Second))
}
