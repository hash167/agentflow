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
	Repo     string   `json:"repo"`
	PRNumber int      `json:"prNumber"`
	Title    string   `json:"title,omitempty"`
	HeadSHA  string   `json:"headSha"`
	Draft    bool     `json:"draft,omitempty"`
	Labels   []string `json:"labels,omitempty"`
}

// ReviewTarget is one (PR, label rule) pair: a PR with both review labels
// yields two targets, each with its own workflow, model and comment.
type ReviewTarget struct {
	PRInfo
	LabelKey string `json:"labelKey"`
	Label    string `json:"label"`
	Model    string `json:"model"`
}

func (t ReviewTarget) Key() string { return ReviewWorkflowID(t.Repo, t.PRNumber, t.LabelKey) }

// Reviewed is the last (head SHA, model) reviewed or started for a target.
type Reviewed struct {
	HeadSHA string `json:"headSha"`
	Model   string `json:"model"`
}

// PollSummary is the PRPollerWorkflow result (also logged).
type PollSummary struct {
	Repos     []string `json:"repos"`
	OpenPRs   int      `json:"openPRs"`
	Unlabeled int      `json:"unlabeled"` // open PRs without a review label (skipped)
	Targets   int      `json:"targets"`   // (PR, label) pairs considered
	Triggered []string `json:"triggered"`
	Unchanged int      `json:"unchanged"`
}

// ListOpenPRsResult is returned by the ListOpenPRs activity.
type ListOpenPRsResult struct {
	Repos   []string       `json:"repos"`
	PRs     []PRInfo       `json:"prs"`
	Targets []ReviewTarget `json:"targets"`
}

// TargetsForPRs applies label gating: unlabeled PRs produce no targets.
func TargetsForPRs(prs []PRInfo, rules []LabelRule) []ReviewTarget {
	out := []ReviewTarget{}
	for _, pr := range prs {
		for _, r := range MatchLabels(rules, pr.Labels) {
			out = append(out, ReviewTarget{PRInfo: pr, LabelKey: r.Key, Label: r.Label, Model: r.Model})
		}
	}
	return out
}

// TargetsNeedingReview is the dedupe rule: a target needs a review only when
// its (head SHA, model) differs from the last one reviewed or started.
func TargetsNeedingReview(targets []ReviewTarget, last map[string]Reviewed) []ReviewTarget {
	var out []ReviewTarget
	for _, t := range targets {
		if t.HeadSHA == "" {
			continue
		}
		if prev, ok := last[t.Key()]; ok && prev.HeadSHA != "" &&
			strings.EqualFold(prev.HeadSHA, t.HeadSHA) && strings.EqualFold(prev.Model, t.Model) {
			continue
		}
		out = append(out, t)
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
	summary := PollSummary{Repos: listed.Repos, OpenPRs: len(listed.PRs), Targets: len(listed.Targets), Triggered: []string{}}
	labelled := map[string]bool{}
	for _, t := range listed.Targets {
		labelled[WorkflowID(t.Repo, t.PRNumber)] = true
	}
	summary.Unlabeled = len(listed.PRs) - len(labelled)
	if len(listed.Targets) > 0 {
		last := map[string]Reviewed{}
		if err := workflow.ExecuteActivity(actx, a.LastReviewedTargets, listed.Targets).Get(ctx, &last); err != nil {
			return summary, err
		}
		todo := TargetsNeedingReview(listed.Targets, last)
		summary.Unchanged = len(listed.Targets) - len(todo)
		if len(todo) > 0 {
			if err := workflow.ExecuteActivity(actx, a.StartReviewTargets, todo).Get(ctx, &summary.Triggered); err != nil {
				return summary, err
			}
		}
	}
	workflow.GetLogger(ctx).Info("pr-poller run",
		"repos", len(summary.Repos), "openPRs", summary.OpenPRs, "unlabeled", summary.Unlabeled, "targets", summary.Targets,
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
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
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
			// Any non-rate-limit 4xx (no access, empty/blocked repo...) only
			// skips that repo; it must not fail the whole poll.
			if errors.As(err, &ghErr) && !ghErr.RateLimited && ghErr.Status >= 400 && ghErr.Status < 500 {
				activity.GetLogger(ctx).Warn("skipping repo", "repo", repo, "error", err.Error())
				continue
			}
			return result, err
		}
		for _, p := range pulls {
			if p.Draft && !cfg.IncludeDrafts {
				continue
			}
			labels := []string{}
			for _, l := range p.Labels {
				labels = append(labels, l.Name)
			}
			result.PRs = append(result.PRs, PRInfo{Repo: repo, PRNumber: p.Number, Title: p.Title, HeadSHA: p.Head.SHA, Draft: p.Draft, Labels: labels})
		}
	}
	result.Targets = TargetsForPRs(result.PRs, a.Review.Labels)
	return result, nil
}

// LastReviewedTargets returns, per target workflow ID, the (head SHA, model)
// last reviewed or currently under review: first from the target workflow's
// status query (running and closed executions within retention), then from
// the head/model markers in that label's comment. Comments without a model
// marker (pre-label reviews) do not count, so labelling a PR reviews it.
func (a *Activities) LastReviewedTargets(ctx context.Context, targets []ReviewTarget) (map[string]Reviewed, error) {
	out := map[string]Reviewed{}
	for _, t := range targets {
		activity.RecordHeartbeat(ctx, t.Key())
		if r := a.reviewedFromWorkflow(ctx, t.Key()); r.HeadSHA != "" {
			out[t.Key()] = r
			continue
		}
		if a.Review.Sink.Type == "github" {
			gh, err := a.github()
			if err != nil {
				return nil, err
			}
			comment, err := gh.FindReviewComment(ctx, t.Repo, t.PRNumber, MarkerFor(t.LabelKey))
			if err != nil {
				return nil, asTemporalError(err)
			}
			if comment != nil {
				if sha, model := HeadSHAFromComment(comment.Body), ModelFromComment(comment.Body); sha != "" && model != "" {
					out[t.Key()] = Reviewed{HeadSHA: sha, Model: model}
				}
			}
		}
	}
	return out, nil
}

func (a *Activities) reviewedFromWorkflow(ctx context.Context, workflowID string) Reviewed {
	if a.Temporal == nil {
		return Reviewed{}
	}
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := a.Temporal.QueryWorkflow(qctx, workflowID, "", QueryStatus)
	if err != nil {
		var notFound *serviceerror.NotFound
		if !errors.As(err, &notFound) {
			activity.GetLogger(ctx).Warn("status query failed", "workflow", workflowID, "error", err.Error())
		}
		return Reviewed{}
	}
	var status Status
	if err := resp.Get(&status); err != nil {
		return Reviewed{}
	}
	if status.HeadSHA != "" {
		return Reviewed{HeadSHA: status.HeadSHA, Model: status.Model}
	}
	if status.LastResult != nil {
		return Reviewed{HeadSHA: status.LastResult.HeadSHA, Model: status.LastResult.Model}
	}
	return Reviewed{}
}

// StartReviewTargets signal-with-starts one review workflow per target
// (same request shape as the webhook) with a GitHub API diffUrl.
func (a *Activities) StartReviewTargets(ctx context.Context, targets []ReviewTarget) ([]string, error) {
	if a.Temporal == nil {
		return nil, temporal.NewNonRetryableApplicationError("StartReviewTargets needs a Temporal client", "NoTemporalClient", nil)
	}
	started := []string{}
	for _, t := range targets {
		request := ReviewRequest{
			Repo:     t.Repo,
			PRNumber: t.PRNumber,
			Title:    t.Title,
			HeadSHA:  t.HeadSHA,
			DiffURL:  fmt.Sprintf("%s/repos/%s/pulls/%d", a.apiBase(), t.Repo, t.PRNumber),
			LabelKey: t.LabelKey,
			Label:    t.Label,
			Model:    t.Model,
		}
		if err := StartReview(ctx, a.Temporal, a.TaskQueue, t.Key(), request); err != nil {
			return started, err
		}
		started = append(started, fmt.Sprintf("%s#%d:%s@%.7s(%s)", t.Repo, t.PRNumber, t.LabelKey, t.HeadSHA, t.Model))
	}
	return started, nil
}

// StartReview signal-with-starts a review workflow; a running execution gets
// the request as a new-push signal instead of a duplicate run.
func StartReview(ctx context.Context, c client.Client, taskQueue, workflowID string, request ReviewRequest) error {
	_, err := c.SignalWithStartWorkflow(ctx, workflowID, SignalNewPush, request,
		client.StartWorkflowOptions{
			ID:                    workflowID,
			TaskQueue:             taskQueue,
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		},
		CodeReviewWorkflow, request)
	return err
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
