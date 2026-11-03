package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

func TestPRsNeedingReview(t *testing.T) {
	prs := []PRInfo{
		{Repo: "o/a", PRNumber: 1, HeadSHA: "aaa"}, // unchanged
		{Repo: "o/a", PRNumber: 2, HeadSHA: "bbb"}, // new push
		{Repo: "o/b", PRNumber: 3, HeadSHA: "ccc"}, // never reviewed
		{Repo: "o/b", PRNumber: 4, HeadSHA: ""},    // no SHA: ignore
	}
	last := map[string]string{
		WorkflowID("o/a", 1): "AAA",
		WorkflowID("o/a", 2): "old",
	}
	got := PRsNeedingReview(prs, last)
	require.Equal(t, []PRInfo{prs[1], prs[2]}, got)
}

func runPoller(t *testing.T, listed ListOpenPRsResult, last map[string]string, expectStart []PRInfo) PollSummary {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	a := &Activities{}
	env.RegisterActivity(a)
	env.OnActivity(a.ListOpenPRs, mock.Anything).Return(listed, nil).Once()
	env.OnActivity(a.LastReviewedSHAs, mock.Anything, listed.PRs).Return(last, nil).Once()
	if expectStart != nil {
		triggered := []string{}
		for _, pr := range expectStart {
			triggered = append(triggered, pr.Key())
		}
		env.OnActivity(a.StartReviews, mock.Anything, expectStart).Return(triggered, nil).Once()
	}
	env.ExecuteWorkflow(PRPollerWorkflow)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var summary PollSummary
	require.NoError(t, env.GetWorkflowResult(&summary))
	env.AssertExpectations(t)
	return summary
}

func TestPRPollerReviewsOnlyNewSHAsThenIdles(t *testing.T) {
	listed := ListOpenPRsResult{
		Repos: []string{"o/a", "o/b"},
		PRs: []PRInfo{
			{Repo: "o/a", PRNumber: 1, HeadSHA: "aaa"},
			{Repo: "o/b", PRNumber: 2, HeadSHA: "bbb"},
		},
	}
	// Run 1: PR 1 already reviewed at aaa, PR 2 is new.
	first := runPoller(t, listed, map[string]string{WorkflowID("o/a", 1): "aaa"}, []PRInfo{listed.PRs[1]})
	require.Equal(t, []string{WorkflowID("o/b", 2)}, first.Triggered)
	require.Equal(t, 1, first.Unchanged)

	// Run 2: both SHAs now recorded; StartReviews must not be called at all.
	second := runPoller(t, listed, map[string]string{
		WorkflowID("o/a", 1): "aaa", WorkflowID("o/b", 2): "bbb",
	}, nil)
	require.Empty(t, second.Triggered)
	require.Equal(t, 2, second.Unchanged)
}

func TestPRPollerNoOpenPRs(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	a := &Activities{}
	env.RegisterActivity(a)
	env.OnActivity(a.ListOpenPRs, mock.Anything).Return(ListOpenPRsResult{Repos: []string{"o/a"}}, nil).Once()
	env.ExecuteWorkflow(PRPollerWorkflow)
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestCodeReviewWorkflowStatusTracksHeadSHA(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	a := &Activities{}
	env.RegisterActivity(a)
	req := ReviewRequest{Repo: "o/r", PRNumber: 9, HeadSHA: "deadbeef", Diff: "+x"}
	res := ReviewResult{Repo: "o/r", PRNumber: 9, HeadSHA: "deadbeef", Summary: "ok", Findings: []ReviewFinding{}}
	env.OnActivity(a.RunReviewAgent, mock.Anything, req).Return(res, nil).Once()
	env.OnActivity(a.PostReview, mock.Anything, res).Return(nil).Once()
	env.ExecuteWorkflow(CodeReviewWorkflow, req)
	require.NoError(t, env.GetWorkflowError())
	v, err := env.QueryWorkflow(QueryStatus)
	require.NoError(t, err)
	var st Status
	require.NoError(t, v.Get(&st))
	require.Equal(t, "deadbeef", st.HeadSHA)
}
