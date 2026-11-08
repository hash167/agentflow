package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

var testRules = []LabelRule{
	{Label: LabelDefault, Key: "default", Model: "grok-4.7"},
	{Label: LabelClaude, Key: "claude", Model: "claude-opus-5-5-high"},
}

func TestTargetsForPRsGatesOnLabels(t *testing.T) {
	prs := []PRInfo{
		{Repo: "o/a", PRNumber: 1, HeadSHA: "aaa"},                                             // unlabeled: skipped
		{Repo: "o/a", PRNumber: 2, HeadSHA: "bbb", Labels: []string{"bug"}},                    // unrelated label
		{Repo: "o/a", PRNumber: 3, HeadSHA: "ccc", Labels: []string{"Agentic-Review-Default"}}, // case-insensitive
		{Repo: "o/b", PRNumber: 4, HeadSHA: "ddd", Labels: []string{LabelClaude, "x", LabelDefault}},
	}
	got := TargetsForPRs(prs, testRules)
	require.Len(t, got, 3)
	require.Equal(t, "review:o/a#3:default", got[0].Key())
	require.Equal(t, "grok-4.7", got[0].Model)
	require.Equal(t, "review:o/b#4:default", got[1].Key())
	require.Equal(t, "review:o/b#4:claude", got[2].Key())
	require.Equal(t, "claude-opus-5-5-high", got[2].Model)
	require.Equal(t, LabelClaude, got[2].Label)
	require.Empty(t, TargetsForPRs(prs[:2], testRules))
}

func TestTargetsNeedingReviewDedupesBySHAAndModel(t *testing.T) {
	pr := PRInfo{Repo: "o/a", PRNumber: 1, HeadSHA: "aaa"}
	def := ReviewTarget{PRInfo: pr, LabelKey: "default", Model: "grok-4.7"}
	cl := ReviewTarget{PRInfo: pr, LabelKey: "claude", Model: "claude-opus-5-5-high"}
	noSHA := ReviewTarget{PRInfo: PRInfo{Repo: "o/a", PRNumber: 2}, LabelKey: "default", Model: "grok-4.7"}
	targets := []ReviewTarget{def, cl, noSHA}

	// Nothing reviewed yet: both labels of the PR are reviewed (first seen).
	require.Equal(t, []ReviewTarget{def, cl}, TargetsNeedingReview(targets, nil, 500_000))
	// Same SHA+model for default; claude only reviewed under another model.
	// A larger budget does not repeat a successful review.
	last := map[string]Reviewed{
		def.Key(): {HeadSHA: "AAA", Model: "GROK-4.7"},
		cl.Key():  {HeadSHA: "aaa", Model: "claude-opus-5-high"},
	}
	require.Equal(t, []ReviewTarget{cl}, TargetsNeedingReview(targets, last, 500_000))
	// New push: both re-reviewed.
	last[cl.Key()] = Reviewed{HeadSHA: "aaa", Model: "claude-opus-5-5-high"}
	require.Empty(t, TargetsNeedingReview(targets, last, 500_000))
	pushed := []ReviewTarget{def, cl}
	pushed[0].HeadSHA, pushed[1].HeadSHA = "bbb", "bbb"
	require.Len(t, TargetsNeedingReview(pushed, last, 300_000), 2)
}

func runPoller(t *testing.T, listed ListOpenPRsResult, last map[string]Reviewed, expectStart []ReviewTarget) PollSummary {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	a := &Activities{}
	env.RegisterActivity(a)
	env.OnActivity(a.ListOpenPRs, mock.Anything).Return(listed, nil).Once()
	if len(listed.Targets) > 0 {
		env.OnActivity(a.LastReviewedTargets, mock.Anything, listed.Targets).Return(last, nil).Once()
	}
	if expectStart != nil {
		triggered := []string{}
		for _, tg := range expectStart {
			triggered = append(triggered, tg.Key())
		}
		env.OnActivity(a.StartReviewTargets, mock.Anything, expectStart).Return(triggered, nil).Once()
	}
	env.ExecuteWorkflow(PRPollerWorkflow)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var summary PollSummary
	require.NoError(t, env.GetWorkflowResult(&summary))
	env.AssertExpectations(t)
	return summary
}

func TestPRPollerReviewsOnlyLabelledNewSHAsThenIdles(t *testing.T) {
	prs := []PRInfo{
		{Repo: "o/a", PRNumber: 1, HeadSHA: "aaa"}, // unlabeled
		{Repo: "o/b", PRNumber: 2, HeadSHA: "bbb", Labels: []string{LabelDefault, LabelClaude}},
	}
	listed := ListOpenPRsResult{Repos: []string{"o/a", "o/b"}, PRs: prs, Targets: TargetsForPRs(prs, testRules)}
	require.Len(t, listed.Targets, 2)

	// Run 1: default already reviewed at bbb with grok; claude is new.
	first := runPoller(t, listed, map[string]Reviewed{
		listed.Targets[0].Key(): {HeadSHA: "bbb", Model: "grok-4.7"},
	}, []ReviewTarget{listed.Targets[1]})
	require.Equal(t, []string{"review:o/b#2:claude"}, first.Triggered)
	require.Equal(t, 1, first.Unchanged)
	require.Equal(t, 1, first.Unlabeled)
	require.Equal(t, 2, first.Targets)

	// Run 2: both recorded; StartReviewTargets must not be called.
	second := runPoller(t, listed, map[string]Reviewed{
		listed.Targets[0].Key(): {HeadSHA: "bbb", Model: "grok-4.7"},
		listed.Targets[1].Key(): {HeadSHA: "bbb", Model: "claude-opus-5-5-high"},
	}, nil)
	require.Empty(t, second.Triggered)
	require.Equal(t, 2, second.Unchanged)
}

func TestPRPollerRetriesBudgetSkipOnlyWhenBudgetIncreases(t *testing.T) {
	pr := PRInfo{Repo: "o/a", PRNumber: 10, HeadSHA: "f6781876f495fef29a49586a6cd96a328f334303", Labels: []string{LabelDefault}}
	listed := ListOpenPRsResult{Repos: []string{pr.Repo}, PRs: []PRInfo{pr}, Targets: TargetsForPRs([]PRInfo{pr}, testRules)}
	require.Len(t, listed.Targets, 1)
	skipped := map[string]Reviewed{
		listed.Targets[0].Key(): {HeadSHA: pr.HeadSHA, Model: "grok-4.7", SkipBudget: 300_000, Phase: "done"},
	}

	same := listed
	same.MaxDiffBytes = 300_000
	held := runPoller(t, same, skipped, nil)
	require.Empty(t, held.Triggered)
	require.Equal(t, 1, held.Unchanged)

	raised := listed
	raised.MaxDiffBytes = 500_000
	retried := runPoller(t, raised, skipped, listed.Targets)
	require.Equal(t, []string{listed.Targets[0].Key()}, retried.Triggered)
	require.Equal(t, 0, retried.Unchanged)
}

func TestPRPollerDoesNotRetryRunningOrSuccessfulReviews(t *testing.T) {
	pr := PRInfo{Repo: "o/a", PRNumber: 10, HeadSHA: "abc", Labels: []string{LabelDefault}}
	listed := ListOpenPRsResult{
		Repos: []string{pr.Repo}, PRs: []PRInfo{pr}, Targets: TargetsForPRs([]PRInfo{pr}, testRules), MaxDiffBytes: 500_000,
	}
	running := runPoller(t, listed, map[string]Reviewed{
		listed.Targets[0].Key(): {HeadSHA: pr.HeadSHA, Model: "grok-4.7", Phase: "reviewing", SkipBudget: 300_000},
	}, nil)
	require.Empty(t, running.Triggered)

	success := runPoller(t, listed, map[string]Reviewed{
		listed.Targets[0].Key(): {HeadSHA: pr.HeadSHA, Model: "grok-4.7", Phase: "done"},
	}, nil)
	require.Empty(t, success.Triggered)
}

func TestPRPollerUnlabeledPRsAreNoOp(t *testing.T) {
	prs := []PRInfo{{Repo: "o/a", PRNumber: 1, HeadSHA: "aaa"}, {Repo: "o/a", PRNumber: 2, HeadSHA: "b", Labels: []string{"bug"}}}
	// No LastReviewedTargets / StartReviewTargets calls are registered: any
	// call fails the test.
	s := runPoller(t, ListOpenPRsResult{Repos: []string{"o/a"}, PRs: prs, Targets: TargetsForPRs(prs, testRules)}, nil, nil)
	require.Equal(t, 2, s.Unlabeled)
	require.Empty(t, s.Triggered)
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
