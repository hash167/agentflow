package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

var _ RetrySources = retryAdapter{}

func TestParseArgs(t *testing.T) {
	got, err := ParseArgs(nil)
	require.NoError(t, err)
	require.Empty(t, got.Command)

	got, err = ParseArgs([]string{"orchestrator.config.json"})
	require.NoError(t, err)
	require.Equal(t, "orchestrator.config.json", got.ConfigPath)

	got, err = ParseArgs([]string{"trigger-poller"})
	require.NoError(t, err)
	require.Equal(t, "trigger-poller", got.Command)

	head := "f6781876f495fef29a49586a6cd96a328f334303"
	got, err = ParseArgs([]string{"trigger-poller", "retry-skipped", "hash167/movie-postcredits", "10", head})
	require.NoError(t, err)
	require.Equal(t, "retry-skipped", got.Command)
	require.Equal(t, "hash167/movie-postcredits", got.Repo)
	require.Equal(t, 10, got.PR)
	require.Equal(t, head, got.HeadSHA)

	for _, args := range [][]string{
		{"trigger-poller", "retry-skipped"},
		{"trigger-poller", "retry-skipped", "movie-postcredits", "10", head},
		{"trigger-poller", "retry-skipped", "hash167/movie-postcredits", "0", head},
		{"trigger-poller", "retry-skipped", "hash167/movie-postcredits", "10", "f6781876"},
		{"trigger-poller", "webhook"},
		{"config.json", "extra"},
	} {
		_, err = ParseArgs(args)
		require.Error(t, err, "%v", args)
	}
}

func TestOperatorExitCodes(t *testing.T) {
	require.Equal(t, 0, (OperatorResult{Status: StatusAccepted}).ExitCode())
	require.Equal(t, 0, (OperatorResult{Status: StatusRunning}).ExitCode())
	require.Equal(t, 2, (OperatorResult{Status: StatusSkipped}).ExitCode())
	require.Equal(t, 1, (OperatorResult{Status: StatusError}).ExitCode())
}

type fakeSched struct {
	desc       *client.ScheduleDescription
	descErr    error
	triggerErr error
	triggers   int
	overlap    enumspb.ScheduleOverlapPolicy
	mutate     func(*client.ScheduleDescription)
}

func (f *fakeSched) Describe(context.Context) (*client.ScheduleDescription, error) {
	if f.descErr != nil {
		return nil, f.descErr
	}
	return f.desc, nil
}

func (f *fakeSched) Trigger(_ context.Context, opt client.ScheduleTriggerOptions) error {
	f.triggers++
	f.overlap = opt.Overlap
	if f.triggerErr != nil {
		return f.triggerErr
	}
	if f.mutate != nil {
		f.mutate(f.desc)
	}
	return nil
}

func TestTriggerPollNowCoalescesAndStarts(t *testing.T) {
	ctx := context.Background()
	_, err := TriggerPollNow(ctx, &fakeSched{}, "")
	require.Error(t, err)

	paused := &fakeSched{desc: &client.ScheduleDescription{Schedule: client.Schedule{State: &client.ScheduleState{Paused: true}}}}
	got, err := TriggerPollNow(ctx, paused, "pr-poller")
	require.NoError(t, err)
	require.Equal(t, StatusSkipped, got.Status)
	require.Equal(t, 0, paused.triggers)

	running := &fakeSched{desc: &client.ScheduleDescription{Info: client.ScheduleInfo{RunningWorkflows: []client.ScheduleWorkflowExecution{{WorkflowID: "pr-poller-run"}}}}}
	got, err = TriggerPollNow(ctx, running, "pr-poller")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, got.Status)
	require.Equal(t, "pr-poller-run", got.WorkflowID)
	require.Equal(t, 0, running.triggers)

	started := &fakeSched{desc: &client.ScheduleDescription{}, mutate: func(d *client.ScheduleDescription) {
		d.Info.NumActions++
		d.Info.RunningWorkflows = []client.ScheduleWorkflowExecution{{WorkflowID: "pr-poller-run"}}
	}}
	got, err = TriggerPollNow(ctx, started, "pr-poller")
	require.NoError(t, err)
	require.Equal(t, StatusAccepted, got.Status)
	require.Equal(t, "pr-poller-run", got.WorkflowID)
	require.Equal(t, 1, started.triggers)
	require.Equal(t, enumspb.SCHEDULE_OVERLAP_POLICY_SKIP, started.overlap)

	raced := &fakeSched{desc: &client.ScheduleDescription{}, mutate: func(d *client.ScheduleDescription) {
		d.Info.NumActionsSkippedOverlap++
		d.Info.RunningWorkflows = []client.ScheduleWorkflowExecution{{WorkflowID: "pr-poller-run"}}
	}}
	got, err = TriggerPollNow(ctx, raced, "pr-poller")
	require.NoError(t, err)
	require.Equal(t, StatusRunning, got.Status)
	require.Equal(t, 1, raced.triggers)

	broken := &fakeSched{descErr: errors.New("temporal down")}
	got, err = TriggerPollNow(ctx, broken, "pr-poller")
	require.Error(t, err)
	require.Equal(t, StatusError, got.Status)
}

type fakeRetry struct {
	info     PRInfo
	state    string
	pullErr  error
	pulls    int
	running  map[string]bool
	records  map[string]Reviewed
	started  []ReviewTarget
	startErr error
}

func (f *fakeRetry) GetPull(context.Context, string, int) (PRInfo, string, error) {
	f.pulls++
	return f.info, f.state, f.pullErr
}

func (f *fakeRetry) WorkflowRunning(_ context.Context, id string) (bool, error) {
	return f.running[id], nil
}

func (f *fakeRetry) ReviewedTarget(_ context.Context, t ReviewTarget) (Reviewed, error) {
	return f.records[t.Key()], nil
}

func (f *fakeRetry) StartTargetReview(_ context.Context, t ReviewTarget) error {
	f.started = append(f.started, t)
	return f.startErr
}

func retryCfg(budget int) *Config {
	return &Config{
		Poller: PollerConfig{Enabled: true, Owner: "hash167", IncludeDrafts: true, ScheduleID: "pr-poller"},
		Review: ReviewConfig{MaxDiffBytes: budget, Labels: testRules},
	}
}

func TestRunRetrySkippedBudgetIncreaseStartsOnlyTheSkippedLabel(t *testing.T) {
	head := "f6781876f495fef29a49586a6cd96a328f334303"
	info := PRInfo{Repo: "hash167/movie-postcredits", PRNumber: 10, Title: "sync", HeadSHA: head, Labels: []string{LabelDefault, LabelClaude}}
	defKey := ReviewWorkflowID(info.Repo, info.PRNumber, "default")
	clKey := ReviewWorkflowID(info.Repo, info.PRNumber, "claude")
	src := &fakeRetry{
		info: info, state: "open",
		records: map[string]Reviewed{
			defKey: {HeadSHA: head, Model: "grok-4.7", SkipBudget: 300_000, Phase: "done"},
			clKey:  {HeadSHA: head, Model: "claude-opus-5-5-high", Phase: "done"},
		},
	}
	got, err := RunRetrySkipped(context.Background(), retryCfg(500_000), info.Repo, 10, head, src)
	require.NoError(t, err)
	require.Equal(t, StatusAccepted, got.Status)
	require.Len(t, src.started, 1)
	require.Equal(t, "default", src.started[0].LabelKey)
	require.Equal(t, head, src.started[0].HeadSHA)
	require.Equal(t, "grok-4.7", src.started[0].Model)
}

func TestRunRetrySkippedRefusesUnchangedBudgetSuccessRunningAndMismatch(t *testing.T) {
	head := "f6781876f495fef29a49586a6cd96a328f334303"
	info := PRInfo{Repo: "hash167/movie-postcredits", PRNumber: 10, HeadSHA: head, Labels: []string{LabelDefault}}
	key := ReviewWorkflowID(info.Repo, 10, "default")
	ctx := context.Background()

	same := &fakeRetry{info: info, state: "open", records: map[string]Reviewed{
		key: {HeadSHA: head, Model: "grok-4.7", SkipBudget: 300_000, Phase: "done"},
	}}
	got, err := RunRetrySkipped(ctx, retryCfg(300_000), info.Repo, 10, head, same)
	require.NoError(t, err)
	require.Equal(t, StatusSkipped, got.Status)
	require.Empty(t, same.started)

	success := &fakeRetry{info: info, state: "open", records: map[string]Reviewed{
		key: {HeadSHA: head, Model: "grok-4.7", Phase: "done"},
	}}
	got, err = RunRetrySkipped(ctx, retryCfg(500_000), info.Repo, 10, head, success)
	require.NoError(t, err)
	require.Equal(t, StatusSkipped, got.Status)
	require.Empty(t, success.started)
	require.Contains(t, got.Targets[0].Detail, "successful reviews are not retried")

	running := &fakeRetry{info: info, state: "open", running: map[string]bool{key: true}, records: map[string]Reviewed{
		key: {HeadSHA: head, Model: "grok-4.7", SkipBudget: 300_000, Phase: "done"},
	}}
	got, err = RunRetrySkipped(ctx, retryCfg(500_000), info.Repo, 10, head, running)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, got.Status)
	require.Empty(t, running.started)

	moved := info
	moved.HeadSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mismatch := &fakeRetry{info: moved, state: "open"}
	got, err = RunRetrySkipped(ctx, retryCfg(500_000), info.Repo, 10, head, mismatch)
	require.NoError(t, err)
	require.Equal(t, StatusError, got.Status)
	require.Empty(t, mismatch.started)

	closed := &fakeRetry{info: info, state: "closed"}
	got, err = RunRetrySkipped(ctx, retryCfg(500_000), info.Repo, 10, head, closed)
	require.NoError(t, err)
	require.Equal(t, StatusError, got.Status)
	require.Empty(t, closed.started)

	unlabeled := info
	unlabeled.Labels = []string{"bug"}
	none := &fakeRetry{info: unlabeled, state: "open"}
	got, err = RunRetrySkipped(ctx, retryCfg(500_000), info.Repo, 10, head, none)
	require.NoError(t, err)
	require.Equal(t, StatusSkipped, got.Status)
	require.Empty(t, none.started)

	cfg := retryCfg(500_000)
	cfg.Poller.IncludeDrafts = false
	draft := &fakeRetry{info: PRInfo{Repo: info.Repo, PRNumber: 10, HeadSHA: head, Draft: true, Labels: []string{LabelDefault}}, state: "open"}
	got, err = RunRetrySkipped(ctx, cfg, info.Repo, 10, head, draft)
	require.NoError(t, err)
	require.Equal(t, StatusSkipped, got.Status)
	require.Empty(t, draft.started)

	denied := retryCfg(500_000)
	denied.Poller.ExcludeRepos = []string{info.Repo}
	excluded := &fakeRetry{info: info, state: "open"}
	got, err = RunRetrySkipped(ctx, denied, info.Repo, 10, head, excluded)
	require.Error(t, err)
	require.Equal(t, StatusError, got.Status)
	require.Equal(t, 0, excluded.pulls)

	off := retryCfg(500_000)
	off.Poller.Enabled = false
	got, err = RunRetrySkipped(ctx, off, info.Repo, 10, head, &fakeRetry{})
	require.Error(t, err)
	require.Equal(t, StatusError, got.Status)
}
