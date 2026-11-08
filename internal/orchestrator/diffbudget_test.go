package orchestrator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

func executeReview(t *testing.T, max int, diff string, command []string) ReviewResult {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	activities := &Activities{Review: ReviewConfig{MaxDiffBytes: max, ReviewerCommand: command}}
	env.RegisterActivity(activities)
	future, err := env.ExecuteActivity(activities.RunReviewAgent, ReviewRequest{
		Repo: "o/r", PRNumber: 1, Diff: diff,
	})
	require.NoError(t, err)
	var result ReviewResult
	require.NoError(t, future.Get(&result))
	return result
}

func TestDiffBudgetBoundaryReviewsExactFit(t *testing.T) {
	const budget = 32
	diff := strings.Repeat("a", budget)
	result := executeReview(t, budget, diff, []string{"sh", "-c", `cat >/dev/null; echo '{"summary":"reviewed","findings":[]}'`})
	require.Empty(t, result.Skipped)
	require.Equal(t, "reviewed", result.Summary)
}

func TestDiffBudgetExceedSkipsKnownSizeWithoutReviewer(t *testing.T) {
	const budget = 32
	diff := strings.Repeat("b", budget+25)
	result := executeReview(t, budget, diff, []string{"/nonexistent"})
	require.Equal(t, "diff is 57 bytes, over the 32 byte budget", result.Skipped)
	require.Equal(t, "Review skipped: "+result.Skipped, result.Summary)
	require.Empty(t, result.Findings)
}

func TestDiffBudgetFetchCapSkipsWithoutReviewingPartial(t *testing.T) {
	const budget = 32
	// The fetch cap is budget+1. That length must skip even though a reviewer
	// command is configured; the command would fail if it were started.
	capped := strings.Repeat("c", budget+1)
	result := executeReview(t, budget, capped, []string{"/nonexistent"})
	require.Equal(t, "diff exceeds the 32 byte budget (read 33 bytes, the fetch cap; full size may be larger; truncated diff not reviewed)", result.Skipped)
	require.Contains(t, result.Summary, "truncated diff not reviewed")
	require.Empty(t, result.Findings)
}

func serveDiff(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchDiffReturnsFullBodyAtBudget(t *testing.T) {
	const budget = 64
	full := strings.Repeat("d", budget)
	srv := serveDiff(t, full)
	activities := &Activities{Review: ReviewConfig{MaxDiffBytes: budget}}
	got, err := activities.FetchDiff(context.Background(), ReviewRequest{DiffURL: srv.URL})
	require.NoError(t, err)
	require.Equal(t, full, got)
	require.Empty(t, diffOverBudget(got, budget))
}

func TestFetchDiffTruncatesAtCapAndReviewSkipsPartial(t *testing.T) {
	const budget = 64
	full := strings.Repeat("e", budget+40)
	srv := serveDiff(t, full)
	activities := &Activities{Review: ReviewConfig{
		MaxDiffBytes:    budget,
		ReviewerCommand: []string{"/nonexistent"},
	}}
	got, err := activities.FetchDiff(context.Background(), ReviewRequest{DiffURL: srv.URL})
	require.NoError(t, err)
	require.Equal(t, budget+1, len(got))
	require.Equal(t, full[:budget+1], got)
	require.NotEqual(t, full, got)

	result := executeReview(t, budget, got, []string{"/nonexistent"})
	require.Contains(t, result.Skipped, "truncated diff not reviewed")
	require.NotContains(t, result.Skipped, "diff is 104 bytes")
	require.Empty(t, result.Findings)
}
