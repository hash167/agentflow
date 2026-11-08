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
// SkipBudget is non-zero only when that attempt finished as a diff-budget
// skip under that many bytes. Phase is set from a workflow query; a comment
// fallback leaves it empty.
type Reviewed struct {
	HeadSHA    string `json:"headSha"`
	Model      string `json:"model"`
	SkipBudget int    `json:"skipBudget,omitempty"`
	Phase      string `json:"phase,omitempty"`
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
	// MaxDiffBytes is the worker's review budget at poll time. The workflow
	// uses it to decide whether a finished diff-budget skip may run once more.
	MaxDiffBytes int `json:"maxDiffBytes"`
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

// TargetsNeedingReview is the dedupe rule. A target is reviewed when its
// (head SHA, model) is new. A finished diff-budget skip of the same head and
// model is reviewed once more only when maxDiffBytes is strictly greater than
// the budget that produced the skip. The same budget does not retry. An
// in-progress review (any phase other than done) is left alone. A successful
// review is not repeated because the budget later grew.
func TargetsNeedingReview(targets []ReviewTarget, last map[string]Reviewed, maxDiffBytes int) []ReviewTarget {
	var out []ReviewTarget
	for _, t := range targets {
		if t.HeadSHA == "" {
			continue
		}
		prev, ok := last[t.Key()]
		if ok && prev.HeadSHA != "" &&
			strings.EqualFold(prev.HeadSHA, t.HeadSHA) && strings.EqualFold(prev.Model, t.Model) {
			if prev.Phase != "" && prev.Phase != "done" {
				continue
			}
			if prev.SkipBudget > 0 && maxDiffBytes > prev.SkipBudget {
				out = append(out, t)
			}
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
		todo := TargetsNeedingReview(listed.Targets, last, listed.MaxDiffBytes)
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
	result := ListOpenPRsResult{Repos: []string{}, PRs: []PRInfo{}, MaxDiffBytes: a.Review.MaxDiffBytes}
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
// A finished diff-budget skip also carries the budget that skipped it.
func (a *Activities) LastReviewedTargets(ctx context.Context, targets []ReviewTarget) (map[string]Reviewed, error) {
	out := map[string]Reviewed{}
	for _, t := range targets {
		activity.RecordHeartbeat(ctx, t.Key())
		rec, err := a.reviewedTarget(ctx, t)
		if err != nil {
			return nil, err
		}
		if rec.HeadSHA != "" {
			out[t.Key()] = rec
		}
	}
	return out, nil
}

type workflowQuery int

const (
	queryNotFound workflowQuery = iota
	queryFound
	queryUnknown
)

// reviewedTarget resolves one target from its workflow query, then its comment.
// A query that fails for a reason other than not-found still uses the comment
// for the head and model, but drops the skip budget so a blip cannot retry.
func (a *Activities) reviewedTarget(ctx context.Context, t ReviewTarget) (Reviewed, error) {
	rec, q := a.reviewedFromWorkflow(ctx, t.Key())
	if q == queryFound && rec.HeadSHA != "" {
		return rec, nil
	}
	comment := Reviewed{}
	if a.Review.Sink.Type == "github" {
		gh, err := a.github()
		if err != nil {
			return Reviewed{}, err
		}
		found, err := gh.FindReviewComment(ctx, t.Repo, t.PRNumber, MarkerFor(t.LabelKey))
		if err != nil {
			return Reviewed{}, asTemporalError(err)
		}
		if found != nil {
			comment = reviewedFromComment(found.Body)
		}
	}
	return mergeReviewRecord(rec, q, comment), nil
}

func (a *Activities) reviewedFromWorkflow(ctx context.Context, workflowID string) (Reviewed, workflowQuery) {
	if a.Temporal == nil {
		return Reviewed{}, queryNotFound
	}
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := a.Temporal.QueryWorkflow(qctx, workflowID, "", QueryStatus)
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return Reviewed{}, queryNotFound
		}
		if activity.IsActivity(ctx) {
			activity.GetLogger(ctx).Warn("status query failed", "workflow", workflowID, "error", err.Error())
		}
		return Reviewed{}, queryUnknown
	}
	var status Status
	if err := resp.Get(&status); err != nil {
		return Reviewed{}, queryUnknown
	}
	return reviewedFromStatus(status), queryFound
}

// reviewedFromStatus copies the workflow query. SkipBudget is set only when
// the cycle finished and its result is for this same head and model, so an
// in-flight review is not restarted and a newer head does not inherit an
// older skip.
func reviewedFromStatus(status Status) Reviewed {
	head, model := status.HeadSHA, status.Model
	if head == "" && status.LastResult != nil {
		head = status.LastResult.HeadSHA
		if model == "" {
			model = status.LastResult.Model
		}
	}
	if head == "" {
		return Reviewed{}
	}
	rec := Reviewed{HeadSHA: head, Model: model, Phase: status.Phase}
	if status.Phase != "done" || status.LastResult == nil {
		return rec
	}
	result := status.LastResult
	if !strings.EqualFold(result.HeadSHA, head) {
		return rec
	}
	if result.Model != "" && !strings.EqualFold(result.Model, model) {
		return rec
	}
	rec.SkipBudget = budgetSkipLimit(result.Skipped)
	return rec
}

func reviewedFromComment(body string) Reviewed {
	sha, model := HeadSHAFromComment(body), ModelFromComment(body)
	if sha == "" || model == "" {
		return Reviewed{}
	}
	return Reviewed{HeadSHA: sha, Model: model, SkipBudget: budgetSkipLimit(body)}
}

func mergeReviewRecord(fromWorkflow Reviewed, q workflowQuery, comment Reviewed) Reviewed {
	if q == queryFound && fromWorkflow.HeadSHA != "" {
		return fromWorkflow
	}
	if q == queryUnknown {
		comment.SkipBudget = 0
		comment.Phase = ""
	}
	if fromWorkflow.HeadSHA != "" {
		return fromWorkflow
	}
	return comment
}

// StartReviewTargets signal-with-starts one review workflow per target
// (same request shape as the webhook) with a GitHub API diffUrl.
func (a *Activities) StartReviewTargets(ctx context.Context, targets []ReviewTarget) ([]string, error) {
	if a.Temporal == nil {
		return nil, temporal.NewNonRetryableApplicationError("StartReviewTargets needs a Temporal client", "NoTemporalClient", nil)
	}
	started := []string{}
	for _, t := range targets {
		if err := StartReview(ctx, a.Temporal, a.TaskQueue, t.Key(), reviewRequestForTarget(a.apiBase(), t)); err != nil {
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
