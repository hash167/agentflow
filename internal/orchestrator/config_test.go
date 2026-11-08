package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestWorkerOptionsDefaultCap(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["/app/mock-reviewer"]}}`))
	require.NoError(t, err)
	require.Equal(t, 4, cfg.Worker.MaxConcurrentActivities)
	opts := WorkerOptions(cfg.Worker)
	require.Equal(t, 4, opts.MaxConcurrentActivityExecutionSize)
	require.Equal(t, 0, opts.MaxConcurrentWorkflowTaskExecutionSize)
	require.False(t, cfg.Poller.Enabled)
	require.Equal(t, "pr-poller", cfg.Poller.ScheduleID)
	require.True(t, cfg.Poller.IncludeDrafts)
}

func TestWorkerOptionsFromConfig(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"]},"worker":{"maxConcurrentActivities":2,"maxConcurrentWorkflowTasks":8}}`))
	require.NoError(t, err)
	opts := WorkerOptions(cfg.Worker)
	require.Equal(t, 2, opts.MaxConcurrentActivityExecutionSize)
	require.Equal(t, 8, opts.MaxConcurrentWorkflowTaskExecutionSize)
}

func TestWorkerOptionsRejectNegative(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"]},"worker":{"maxConcurrentActivities":-1}}`))
	require.Error(t, err)
}

func TestPollerConfigValidation(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"]},"poller":{"enabled":true,"interval":"10s"}}`))
	require.Error(t, err)
	cfg, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"]},"poller":{"enabled":true,"includeDrafts":false,"excludeRepos":["junk"]}}`))
	require.NoError(t, err)
	require.Equal(t, "15m", cfg.Poller.Interval)
	require.Equal(t, "hash167", cfg.Poller.Owner)
	require.False(t, cfg.Poller.IncludeDrafts)
}

func TestMaxDiffBytesDefaultLeavesOtherCapsAlone(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"]}}`))
	require.NoError(t, err)
	require.Equal(t, DefaultMaxDiffBytes, cfg.Review.MaxDiffBytes)
	require.Equal(t, 500_000, cfg.Review.MaxDiffBytes)
	reviewer, activity, err := cfg.Review.Timeouts()
	require.NoError(t, err)
	require.Equal(t, DefaultReviewerTimeout, reviewer)
	require.Equal(t, DefaultActivityTimeout, activity)
	require.Equal(t, DefaultMaxConcurrentActivities, cfg.Worker.MaxConcurrentActivities)
}

func TestMaxDiffBytesOverrideLeavesTimeoutsAlone(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"],"maxDiffBytes":123456,"reviewerTimeout":"12m","activityTimeout":"15m"},"worker":{"maxConcurrentActivities":4}}`))
	require.NoError(t, err)
	require.Equal(t, 123456, cfg.Review.MaxDiffBytes)
	reviewer, activity, err := cfg.Review.Timeouts()
	require.NoError(t, err)
	require.Equal(t, 12*time.Minute, reviewer)
	require.Equal(t, 15*time.Minute, activity)
	require.Equal(t, 4, cfg.Worker.MaxConcurrentActivities)
}

func TestReviewTimeoutDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"]}}`))
	require.NoError(t, err)
	r, a, err := cfg.Review.Timeouts()
	require.NoError(t, err)
	require.Equal(t, 12*time.Minute, r)
	require.Equal(t, 15*time.Minute, a)
	require.Equal(t, "12m0s", cfg.Review.ReviewerTimeout)
	require.Equal(t, "15m0s", cfg.Review.ActivityTimeout)
}

func TestReviewTimeoutsFromConfig(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"review":{"reviewerCommand":["x"],"reviewerTimeout":"20m","activityTimeout":"25m"}}`))
	require.NoError(t, err)
	r, a, err := cfg.Review.Timeouts()
	require.NoError(t, err)
	require.Equal(t, 20*time.Minute, r)
	require.Equal(t, 25*time.Minute, a)
}

func TestReviewTimeoutsRejectActivityNotAboveReviewer(t *testing.T) {
	for _, body := range []string{
		`{"review":{"reviewerCommand":["x"],"reviewerTimeout":"15m","activityTimeout":"15m"}}`,
		`{"review":{"reviewerCommand":["x"],"reviewerTimeout":"20m"}}`,
		`{"review":{"reviewerCommand":["x"],"activityTimeout":"10m"}}`,
		`{"review":{"reviewerCommand":["x"],"reviewerTimeout":"soon"}}`,
		`{"review":{"reviewerCommand":["x"],"activityTimeout":"-5m"}}`,
	} {
		_, err := LoadConfig(writeConfig(t, body))
		require.Error(t, err, body)
	}
}
