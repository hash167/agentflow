package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// PollerConfig drives the PR poller: a Temporal Schedule that periodically
// lists open PRs and starts reviews for new head SHAs (no public webhook).
type PollerConfig struct {
	Enabled    bool   `json:"enabled"`
	ScheduleID string `json:"scheduleId"` // default "pr-poller"
	Interval   string `json:"interval"`   // Go duration, default "15m"
	Owner      string `json:"owner"`      // default "hash167"
	// Repos, if set, is an allowlist of repo names ("name" or "owner/name").
	Repos []string `json:"repos,omitempty"`
	// ExcludeRepos is a denylist applied after the allowlist.
	ExcludeRepos  []string `json:"excludeRepos,omitempty"`
	IncludeDrafts bool     `json:"includeDrafts"` // default true
	// MinRateRemaining stops listing when GitHub's remaining quota drops
	// below this value (default 100).
	MinRateRemaining int `json:"minRateRemaining,omitempty"`
}

func (p PollerConfig) IntervalDuration() (time.Duration, error) {
	d, err := time.ParseDuration(p.Interval)
	if err != nil || d < time.Minute {
		return 0, fmt.Errorf("poller.interval must be a Go duration >= 1m, got %q", p.Interval)
	}
	return d, nil
}

// PRInfo is one open PR as seen by the poller.
type PRInfo struct {
	Repo     string `json:"repo"`
	PRNumber int    `json:"prNumber"`
	Title    string `json:"title,omitempty"`
	HeadSHA  string `json:"headSha"`
	Draft    bool   `json:"draft,omitempty"`
}

func (p PRInfo) Key() string { return WorkflowID(p.Repo, p.PRNumber) }

// PollSummary is the PRPollerWorkflow result (also logged).
type PollSummary struct {
	Repos     []string `json:"repos"`
	OpenPRs   int      `json:"openPRs"`
	Triggered []string `json:"triggered"`
	Unchanged int      `json:"unchanged"`
}

// ListOpenPRsResult is returned by the ListOpenPRs activity.
type ListOpenPRsResult struct {
	Repos []string `json:"repos"`
	PRs   []PRInfo `json:"prs"`
}

// PRsNeedingReview is the dedupe rule: a PR needs a review only when its
// current head SHA differs from the last SHA we reviewed (or started).
func PRsNeedingReview(prs []PRInfo, lastReviewed map[string]string) []PRInfo {
	var out []PRInfo
	for _, pr := range prs {
		if pr.HeadSHA == "" {
			continue
		}
		if last := lastReviewed[pr.Key()]; last != "" && strings.EqualFold(last, pr.HeadSHA) {
			continue
		}
		out = append(out, pr)
	}
	return out
}

// PRPollerWorkflow lists open PRs and signal-with-starts the per-PR review
// workflow only for PRs whose head SHA is new.
func PRPollerWorkflow(ctx workflow.Context) (PollSummary, error) {
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 10 * time.Second, BackoffCoefficient: 2, MaximumAttempts: 3,
		},
	})
	var a *Activities
	var listed ListOpenPRsResult
	if err := workflow.ExecuteActivity(actx, a.ListOpenPRs).Get(ctx, &listed); err != nil {
		return PollSummary{}, err
	}
	summary := PollSummary{Repos: listed.Repos, OpenPRs: len(listed.PRs), Triggered: []string{}}
	if len(listed.PRs) > 0 {
		lastReviewed := map[string]string{}
		if err := workflow.ExecuteActivity(actx, a.LastReviewedSHAs, listed.PRs).Get(ctx, &lastReviewed); err != nil {
			return summary, err
		}
		todo := PRsNeedingReview(listed.PRs, lastReviewed)
		summary.Unchanged = len(listed.PRs) - len(todo)
		if len(todo) > 0 {
			if err := workflow.ExecuteActivity(actx, a.StartReviews, todo).Get(ctx, &summary.Triggered); err != nil {
				return summary, err
			}
		}
	}
	workflow.GetLogger(ctx).Info("pr-poller run",
		"repos", len(summary.Repos), "openPRs", summary.OpenPRs,
		"triggered", strings.Join(summary.Triggered, ","), "unchanged", summary.Unchanged)
	return summary, nil
}

func repoMatches(list []string, owner, fullName string) bool {
	for _, item := range list {
		item = strings.TrimSpace(item)
		if strings.EqualFold(item, fullName) || strings.EqualFold(owner+"/"+item, fullName) {
			return true
		}
	}
	return false
}

type ghRepo struct {
	FullName string `json:"full_name"`
	Archived bool   `json:"archived"`
	Disabled bool   `json:"disabled"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type ghPull struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Draft  bool   `json:"draft"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// ListOpenPRs lists open PRs in the owner's non-archived repos visible to
// GITHUB_TOKEN, filtered by the allow/deny lists and the draft setting.
func (a *Activities) ListOpenPRs(ctx context.Context) (ListOpenPRsResult, error) {
	gh, err := a.github()
	if err != nil {
		return ListOpenPRsResult{}, err
	}
	cfg := a.Poller
	minRemaining := cfg.MinRateRemaining
	if minRemaining <= 0 {
		minRemaining = 100
	}
	repos, err := getAll[ghRepo](ctx, gh, "/user/repos?affiliation=owner,collaborator,organization_member&per_page=100&sort=full_name", "list repos")
	if err != nil {
		return ListOpenPRsResult{}, asTemporalError(err)
	}
	result := ListOpenPRsResult{Repos: []string{}, PRs: []PRInfo{}}
	for _, repo := range repos {
		if !strings.EqualFold(repo.Owner.Login, cfg.Owner) || repo.Archived || repo.Disabled {
			continue
		}
		if len(cfg.Repos) > 0 && !repoMatches(cfg.Repos, cfg.Owner, repo.FullName) {
			continue
		}
		if repoMatches(cfg.ExcludeRepos, cfg.Owner, repo.FullName) {
			continue
		}
		result.Repos = append(result.Repos, repo.FullName)
	}
	sort.Strings(result.Repos)
	for _, repo := range result.Repos {
		if gh.RateRemaining >= 0 && gh.RateRemaining < minRemaining {
			activity.GetLogger(ctx).Warn("github rate limit low; stopping this poll early", "remaining", gh.RateRemaining)
			break
		}
		activity.RecordHeartbeat(ctx, repo)
		pulls, err := getAll[ghPull](ctx, gh, fmt.Sprintf("/repos/%s/pulls?state=open&per_page=100", repo), "list pulls")
		if err != nil {
			var ghErr *GitHubError
			if errors.As(err, &ghErr) && ghErr.Permanent() {
				activity.GetLogger(ctx).Warn("skipping repo", "repo", repo, "error", err.Error())
				continue
			}
			return result, err
		}
		for _, p := range pulls {
			if p.Draft && !cfg.IncludeDrafts {
				continue
			}
			result.PRs = append(result.PRs, PRInfo{Repo: repo, PRNumber: p.Number, Title: p.Title, HeadSHA: p.Head.SHA, Draft: p.Draft})
		}
	}
	return result, nil
}

// LastReviewedSHAs returns, per PR workflow ID, the head SHA last reviewed
// or currently under review: first from the per-PR workflow's status query
// (works for running and closed executions within retention), then from the
// head SHA embedded in our marker comment.
func (a *Activities) LastReviewedSHAs(ctx context.Context, prs []PRInfo) (map[string]string, error) {
	out := map[string]string{}
	for _, pr := range prs {
		activity.RecordHeartbeat(ctx, pr.Key())
		if sha := a.shaFromWorkflow(ctx, pr); sha != "" {
			out[pr.Key()] = sha
			continue
		}
		if a.Review.Sink.Type == "github" {
			gh, err := a.github()
			if err != nil {
				return nil, err
			}
			comment, err := gh.FindReviewComment(ctx, pr.Repo, pr.PRNumber)
			if err != nil {
				return nil, asTemporalError(err)
			}
			if comment != nil {
				if sha := HeadSHAFromComment(comment.Body); sha != "" {
					out[pr.Key()] = sha
				}
			}
		}
	}
	return out, nil
}

func (a *Activities) shaFromWorkflow(ctx context.Context, pr PRInfo) string {
	if a.Temporal == nil {
		return ""
	}
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := a.Temporal.QueryWorkflow(qctx, pr.Key(), "", QueryStatus)
	if err != nil {
		var notFound *serviceerror.NotFound
		if !errors.As(err, &notFound) {
			activity.GetLogger(ctx).Warn("status query failed", "workflow", pr.Key(), "error", err.Error())
		}
		return ""
	}
	var status Status
	if err := resp.Get(&status); err != nil {
		return ""
	}
	if status.HeadSHA != "" {
		return status.HeadSHA
	}
	if status.LastResult != nil {
		return status.LastResult.HeadSHA
	}
	return ""
}

// StartReviews signal-with-starts the per-PR review workflow (same ID and
// request shape as the webhook) with a GitHub API diffUrl.
func (a *Activities) StartReviews(ctx context.Context, prs []PRInfo) ([]string, error) {
	if a.Temporal == nil {
		return nil, temporal.NewNonRetryableApplicationError("StartReviews needs a Temporal client", "NoTemporalClient", nil)
	}
	started := []string{}
	for _, pr := range prs {
		request := ReviewRequest{
			Repo:     pr.Repo,
			PRNumber: pr.PRNumber,
			Title:    pr.Title,
			HeadSHA:  pr.HeadSHA,
			DiffURL:  fmt.Sprintf("%s/repos/%s/pulls/%d", a.apiBase(), pr.Repo, pr.PRNumber),
		}
		_, err := a.Temporal.SignalWithStartWorkflow(ctx, pr.Key(), SignalNewPush, request,
			client.StartWorkflowOptions{
				ID:                    pr.Key(),
				TaskQueue:             a.TaskQueue,
				WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
			},
			CodeReviewWorkflow, request)
		if err != nil {
			return started, err
		}
		started = append(started, fmt.Sprintf("%s#%d@%.7s", pr.Repo, pr.PRNumber, pr.HeadSHA))
	}
	return started, nil
}

// EnsurePollerSchedule creates the poller schedule, or updates it to match
// config if it already exists (idempotent across restarts).
func EnsurePollerSchedule(ctx context.Context, c client.Client, cfg PollerConfig, taskQueue string) error {
	interval, err := cfg.IntervalDuration()
	if err != nil {
		return err
	}
	spec := client.ScheduleSpec{Intervals: []client.ScheduleIntervalSpec{{Every: interval}}}
	action := &client.ScheduleWorkflowAction{
		ID:        cfg.ScheduleID + "-run",
		Workflow:  PRPollerWorkflow,
		TaskQueue: taskQueue,
	}
	_, err = c.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID:      cfg.ScheduleID,
		Spec:    spec,
		Action:  action,
		Overlap: enums.SCHEDULE_OVERLAP_POLICY_SKIP,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, temporal.ErrScheduleAlreadyRunning) {
		return err
	}
	handle := c.ScheduleClient().GetHandle(ctx, cfg.ScheduleID)
	return handle.Update(ctx, client.ScheduleUpdateOptions{
		DoUpdate: func(in client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
			s := in.Description.Schedule
			s.Spec = &spec
			s.Action = action
			if s.Policy == nil {
				s.Policy = &client.SchedulePolicies{}
			}
			s.Policy.Overlap = enums.SCHEDULE_OVERLAP_POLICY_SKIP
			return &client.ScheduleUpdate{Schedule: &s}, nil
		},
	})
}
