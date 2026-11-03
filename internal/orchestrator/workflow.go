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

// reviewActivityTimeout is the RunReviewAgent StartToClose. It is process
// configuration (review.activityTimeout), set once at worker startup via
// SetReviewActivityTimeout before the worker starts. Activity timeouts are not
// part of Temporal's determinism checks, so changing it between deploys is
// replay-safe.
var reviewActivityTimeout = DefaultActivityTimeout

// SetReviewActivityTimeout configures the RunReviewAgent StartToClose. Call it
// before starting the worker; non-positive values keep the default.
func SetReviewActivityTimeout(d time.Duration) {
	if d > 0 {
		reviewActivityTimeout = d
	}
}

// ReviewActivityTimeout returns the configured RunReviewAgent StartToClose.
func ReviewActivityTimeout() time.Duration { return reviewActivityTimeout }

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

	baseOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:    time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    2 * time.Minute,
			MaximumAttempts:    4,
		},
	}
	// FetchDiff and PostReview keep 10m; RunReviewAgent gets the configured
	// (longer) StartToClose so it outlasts the reviewer subprocess timeout.
	activityCtx := workflow.WithActivityOptions(ctx, baseOptions)
	reviewOptions := baseOptions
	reviewOptions.StartToCloseTimeout = reviewActivityTimeout
	reviewCtx := workflow.WithActivityOptions(ctx, reviewOptions)

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
		if err := workflow.ExecuteActivity(reviewCtx, activities.RunReviewAgent, request).
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
