package orchestrator

import (
	"strconv"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// WorkflowID returns the deterministic workflow ID for a PR, so webhook
// retries and new pushes signal the same execution instead of duplicating it.
func WorkflowID(repo string, prNumber int) string {
	return "review:" + repo + "#" + strconv.Itoa(prNumber)
}

// CodeReviewWorkflow reviews a PR and re-reviews whenever a new push arrives
// via the SignalNewPush signal while a cycle is in flight.
func CodeReviewWorkflow(ctx workflow.Context, initial ReviewRequest) (ReviewResult, error) {
	status := Status{Phase: "fetching"}
	if err := workflow.SetQueryHandler(ctx, QueryStatus, func() (Status, error) {
		return status, nil
	}); err != nil {
		return ReviewResult{}, err
	}

	signals := workflow.GetSignalChannel(ctx, SignalNewPush)
	drainLatest := func(current ReviewRequest) ReviewRequest {
		for {
			var next ReviewRequest
			if !signals.ReceiveAsync(&next) {
				return current
			}
			current = next
		}
	}

	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:    time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    2 * time.Minute,
			MaximumAttempts:    4,
		},
	})

	var activities *Activities
	request := initial
	var result ReviewResult

	for {
		request = drainLatest(request)

		status.Phase = "fetching"
		status.HeadSHA = request.HeadSHA
		if request.Diff == "" && request.DiffURL != "" {
			if err := workflow.ExecuteActivity(activityCtx, activities.FetchDiff, request).
				Get(ctx, &request.Diff); err != nil {
				return ReviewResult{}, err
			}
		}

		status.Phase = "reviewing"
		if err := workflow.ExecuteActivity(activityCtx, activities.RunReviewAgent, request).
			Get(ctx, &result); err != nil {
			return ReviewResult{}, err
		}

		status.Phase = "posting"
		if err := workflow.ExecuteActivity(activityCtx, activities.PostReview, result).
			Get(ctx, nil); err != nil {
			return ReviewResult{}, err
		}

		status.Reviews++
		status.LastResult = &result

		// A push that landed mid-review supersedes the result we just posted.
		var next ReviewRequest
		if signals.ReceiveAsync(&next) {
			request = next
			continue
		}
		status.Phase = "done"
		return result, nil
	}
}
