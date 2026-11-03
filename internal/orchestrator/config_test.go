package orchestrator

import (
	"os"
	"path/filepath"
	"testing"

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
