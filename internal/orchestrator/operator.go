package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// Operator statuses are the only values printed for a manual poll or a
// scoped retry. ExitCode maps them for the process.
const (
	StatusAccepted = "accepted"
	StatusRunning  = "running"
	StatusSkipped  = "skipped"
	StatusError    = "error"
)

// OperatorResult is the JSON document printed by the operator commands.
type OperatorResult struct {
	Status     string           `json:"status"`
	Detail     string           `json:"detail,omitempty"`
	ScheduleID string           `json:"scheduleId,omitempty"`
	WorkflowID string           `json:"workflowId,omitempty"`
	Repo       string           `json:"repo,omitempty"`
	PRNumber   int              `json:"prNumber,omitempty"`
	HeadSHA    string           `json:"headSha,omitempty"`
	Targets    []OperatorTarget `json:"targets,omitempty"`
}

// OperatorTarget is one label's outcome inside a scoped retry.
type OperatorTarget struct {
	Label      string `json:"label,omitempty"`
	Model      string `json:"model,omitempty"`
	WorkflowID string `json:"workflowId,omitempty"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
}

// ExitCode returns 0 when a poll or review is running or was started, 2 when
// the command deliberately did nothing, and 1 on error.
func (r OperatorResult) ExitCode() int {
	switch r.Status {
	case StatusAccepted, StatusRunning:
		return 0
	case StatusSkipped:
		return 2
	default:
		return 1
	}
}

// Args is a parsed orchestrator invocation.
type Args struct {
	ConfigPath string
	// Command is "", "trigger-poller", or "retry-skipped".
	Command string
	Repo    string
	PR      int
	HeadSHA string
}

var fullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// ParseArgs classifies process arguments after the program name.
//
//	orchestrator
//	orchestrator <config.json>
//	orchestrator trigger-poller
//	orchestrator trigger-poller retry-skipped <owner/repo> <pr> <40-hex head>
func ParseArgs(args []string) (Args, error) {
	if len(args) == 0 {
		return Args{}, nil
	}
	if args[0] != "trigger-poller" {
		if len(args) != 1 {
			return Args{}, fmt.Errorf("usage: orchestrator [config.json]\n       orchestrator trigger-poller\n       orchestrator trigger-poller retry-skipped <owner/repo> <pr> <40-hex head>")
		}
		return Args{ConfigPath: args[0]}, nil
	}
	if len(args) == 1 {
		return Args{Command: "trigger-poller"}, nil
	}
	if args[1] != "retry-skipped" || len(args) != 5 {
		return Args{}, fmt.Errorf("usage: orchestrator trigger-poller retry-skipped <owner/repo> <pr> <40-hex head>")
	}
	repo := args[2]
	if err := validateRepo(repo); err != nil {
		return Args{}, err
	}
	pr, err := strconv.Atoi(args[3])
	if err != nil || pr <= 0 {
		return Args{}, fmt.Errorf("pr must be a positive integer")
	}
	head := args[4]
	if !fullSHA.MatchString(head) {
		return Args{}, fmt.Errorf("head must be the pull request's current 40-character sha")
	}
	return Args{Command: "retry-skipped", Repo: repo, PR: pr, HeadSHA: head}, nil
}

func validateRepo(repo string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(repo, " \t") {
		return fmt.Errorf("repo must be owner/name")
	}
	return nil
}

// RepoAllowed reports whether a named repo may be polled under cfg.
func RepoAllowed(cfg PollerConfig, fullName string) error {
	if err := validateRepo(fullName); err != nil {
		return err
	}
	owner, _, _ := strings.Cut(fullName, "/")
	if cfg.Owner != "" && !strings.EqualFold(owner, cfg.Owner) {
		return fmt.Errorf("repo owner %s is outside poller.owner %s", owner, cfg.Owner)
	}
	if len(cfg.Repos) > 0 && !repoMatches(cfg.Repos, owner, fullName) {
		return fmt.Errorf("repo is not in the poller allowlist")
	}
	if repoMatches(cfg.ExcludeRepos, owner, fullName) {
		return fmt.Errorf("repo is excluded by the poller")
	}
	return nil
}

// PollSchedule is the slice of a Temporal schedule handle used to kick one poll.
type PollSchedule interface {
	Describe(ctx context.Context) (*client.ScheduleDescription, error)
	Trigger(ctx context.Context, options client.ScheduleTriggerOptions) error
}

// TriggerPollNow starts one normal poller run. Overlap is forced to Skip, and
// an already-running poll is not triggered again. The poll itself still applies
// label, allowlist, and dedupe rules.
func TriggerPollNow(ctx context.Context, sched PollSchedule, scheduleID string) (OperatorResult, error) {
	result := OperatorResult{ScheduleID: scheduleID, Status: StatusError}
	if scheduleID == "" {
		result.Detail = "poller schedule id is empty"
		return result, fmt.Errorf("%s", result.Detail)
	}
	before, err := sched.Describe(ctx)
	if err != nil {
		result.Detail = err.Error()
		return result, err
	}
	if before.Schedule.State != nil && before.Schedule.State.Paused {
		result.Status = StatusSkipped
		result.Detail = "schedule is paused"
		return result, nil
	}
	if len(before.Info.RunningWorkflows) > 0 {
		result.Status = StatusRunning
		result.WorkflowID = before.Info.RunningWorkflows[0].WorkflowID
		result.Detail = "a poll is already running"
		return result, nil
	}
	skippedBefore := before.Info.NumActionsSkippedOverlap
	actionsBefore := before.Info.NumActions
	if err := sched.Trigger(ctx, client.ScheduleTriggerOptions{Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_SKIP}); err != nil {
		result.Detail = err.Error()
		return result, err
	}
	after, err := sched.Describe(ctx)
	if err != nil {
		result.Status = StatusAccepted
		result.Detail = "poll triggered; confirmation failed: " + err.Error()
		return result, nil
	}
	if after.Info.NumActionsSkippedOverlap > skippedBefore {
		result.Status = StatusSkipped
		result.Detail = "trigger coalesced because a poll was already running"
		if len(after.Info.RunningWorkflows) > 0 {
			result.WorkflowID = after.Info.RunningWorkflows[0].WorkflowID
			result.Status = StatusRunning
			result.Detail = "a poll is already running; this trigger was coalesced"
		}
		return result, nil
	}
	result.Status = StatusAccepted
	result.Detail = "poll started"
	if after.Info.NumActions > actionsBefore || len(after.Info.RunningWorkflows) > 0 || len(after.Info.RecentActions) > 0 {
		if len(after.Info.RunningWorkflows) > 0 {
			result.WorkflowID = after.Info.RunningWorkflows[len(after.Info.RunningWorkflows)-1].WorkflowID
		} else if n := len(after.Info.RecentActions); n > 0 && after.Info.RecentActions[n-1].StartWorkflowResult != nil {
			result.WorkflowID = after.Info.RecentActions[n-1].StartWorkflowResult.WorkflowID
		}
	}
	return result, nil
}

// RetrySources is the outbound read and single-workflow start used by a scoped retry.
type RetrySources interface {
	GetPull(ctx context.Context, repo string, pr int) (PRInfo, string, error)
	WorkflowRunning(ctx context.Context, workflowID string) (bool, error)
	ReviewedTarget(ctx context.Context, t ReviewTarget) (Reviewed, error)
	StartTargetReview(ctx context.Context, t ReviewTarget) error
}

// retryAdapter exposes poller reads to the operator command without adding
// extra methods to Activities. Temporal registers every exported Activities
// method, and GetPull's three results are not an activity signature.
type retryAdapter struct{ *Activities }

// NewRetrySource adapts a worker's activities for RunRetrySkipped.
func NewRetrySource(a *Activities) RetrySources { return retryAdapter{a} }

func (s retryAdapter) GetPull(ctx context.Context, repo string, pr int) (PRInfo, string, error) {
	gh, err := s.github()
	if err != nil {
		return PRInfo{}, "", err
	}
	return gh.GetPull(ctx, repo, pr)
}

func (s retryAdapter) WorkflowRunning(ctx context.Context, workflowID string) (bool, error) {
	return s.workflowRunning(ctx, workflowID)
}

func (s retryAdapter) ReviewedTarget(ctx context.Context, t ReviewTarget) (Reviewed, error) {
	return s.reviewedTarget(ctx, t)
}

func (s retryAdapter) StartTargetReview(ctx context.Context, t ReviewTarget) error {
	return s.startTargetReview(ctx, t)
}

// RetrySkippedInput is the pure input to PlanRetrySkipped.
type RetrySkippedInput struct {
	RequestedHead string
	CurrentHead   string
	State         string
	Draft         bool
	IncludeDrafts bool
	Labels        []string
	Rules         []LabelRule
	Records       map[string]Reviewed
	MaxDiffBytes  int
	RepoAllowed   bool
	RepoDeny      string
}

// RetrySkippedPlan is the decision. Start is the only set that may be started.
type RetrySkippedPlan struct {
	Result OperatorResult
	Start  []ReviewTarget
}

// PlanRetrySkipped decides a scoped retry. It never clears workflow history or
// comments. A successful head is not started. A diff-budget skip is started
// only when the current budget is strictly greater than the budget that skipped.
func PlanRetrySkipped(repo string, pr int, title string, in RetrySkippedInput) RetrySkippedPlan {
	base := OperatorResult{Status: StatusError, Repo: repo, PRNumber: pr, HeadSHA: in.RequestedHead}
	if !in.RepoAllowed {
		detail := in.RepoDeny
		if detail == "" {
			detail = "repo is not visible to the poller"
		}
		base.Detail = detail
		return RetrySkippedPlan{Result: base}
	}
	if in.State != "" && !strings.EqualFold(in.State, "open") {
		base.Detail = "pull request is not open"
		return RetrySkippedPlan{Result: base}
	}
	if !strings.EqualFold(in.RequestedHead, in.CurrentHead) {
		base.Detail = "requested head is not the pull request's current head"
		return RetrySkippedPlan{Result: base}
	}
	if in.Draft && !in.IncludeDrafts {
		base.Status = StatusSkipped
		base.Detail = "draft pull requests are excluded"
		return RetrySkippedPlan{Result: base}
	}
	rules := MatchLabels(in.Rules, in.Labels)
	if len(rules) == 0 {
		base.Status = StatusSkipped
		base.Detail = "pull request has no review label"
		return RetrySkippedPlan{Result: base}
	}
	plan := RetrySkippedPlan{Result: base}
	for _, rule := range rules {
		target := ReviewTarget{
			PRInfo:   PRInfo{Repo: repo, PRNumber: pr, Title: title, HeadSHA: in.CurrentHead},
			LabelKey: rule.Key, Label: rule.Label, Model: rule.Model,
		}
		rec := in.Records[target.Key()]
		decision := decideBudgetSkipRetry(target, rec, in.MaxDiffBytes)
		plan.Result.Targets = append(plan.Result.Targets, decision)
		if decision.Status == StatusAccepted {
			plan.Start = append(plan.Start, target)
		}
	}
	plan.Result.Status = overallStatus(plan.Result.Targets)
	plan.Result.Detail = overallDetail(plan.Result.Targets)
	if len(plan.Start) == 1 {
		plan.Result.WorkflowID = plan.Start[0].Key()
	}
	return plan
}

func decideBudgetSkipRetry(target ReviewTarget, rec Reviewed, maxDiffBytes int) OperatorTarget {
	out := OperatorTarget{Label: target.Label, Model: target.Model, WorkflowID: target.Key()}
	if rec.Phase != "" && rec.Phase != "done" {
		out.Status = StatusRunning
		out.Detail = "review is already in progress"
		return out
	}
	same := rec.HeadSHA != "" && strings.EqualFold(rec.HeadSHA, target.HeadSHA) && strings.EqualFold(rec.Model, target.Model)
	if !same || rec.SkipBudget <= 0 {
		out.Status = StatusSkipped
		out.Detail = "no diff-budget skip for this head and model; successful reviews are not retried"
		return out
	}
	if maxDiffBytes <= rec.SkipBudget {
		out.Status = StatusSkipped
		out.Detail = fmt.Sprintf("review budget %d has not increased past the skip budget %d", maxDiffBytes, rec.SkipBudget)
		return out
	}
	out.Status = StatusAccepted
	out.Detail = fmt.Sprintf("diff-budget skip at %d bytes; current budget is %d", rec.SkipBudget, maxDiffBytes)
	return out
}

func overallStatus(targets []OperatorTarget) string {
	accepted, running, errored := false, false, false
	for _, t := range targets {
		switch t.Status {
		case StatusAccepted:
			accepted = true
		case StatusRunning:
			running = true
		case StatusError:
			errored = true
		}
	}
	if accepted {
		return StatusAccepted
	}
	if errored {
		return StatusError
	}
	if running {
		return StatusRunning
	}
	return StatusSkipped
}

func overallDetail(targets []OperatorTarget) string {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, t.Model+": "+t.Status)
	}
	return strings.Join(parts, "; ")
}

// RunRetrySkipped reads one PR and starts only the plan's accepted targets.
// It does not terminate workflows, delete comments, or edit history.
func RunRetrySkipped(ctx context.Context, cfg *Config, repo string, pr int, head string, src RetrySources) (OperatorResult, error) {
	result := OperatorResult{Status: StatusError, Repo: repo, PRNumber: pr, HeadSHA: head}
	if cfg == nil || !cfg.Poller.Enabled {
		result.Detail = "poller is disabled"
		return result, fmt.Errorf("%s", result.Detail)
	}
	if err := RepoAllowed(cfg.Poller, repo); err != nil {
		result.Detail = err.Error()
		return result, err
	}
	info, state, err := src.GetPull(ctx, repo, pr)
	if err != nil {
		result.Detail = err.Error()
		return result, err
	}
	records := map[string]Reviewed{}
	rules := MatchLabels(cfg.Review.Labels, info.Labels)
	for _, rule := range rules {
		target := ReviewTarget{
			PRInfo:   PRInfo{Repo: repo, PRNumber: pr, HeadSHA: info.HeadSHA},
			LabelKey: rule.Key, Label: rule.Label, Model: rule.Model,
		}
		running, err := src.WorkflowRunning(ctx, target.Key())
		if err != nil {
			result.Detail = err.Error()
			return result, err
		}
		rec, err := src.ReviewedTarget(ctx, target)
		if err != nil {
			result.Detail = err.Error()
			return result, err
		}
		if running {
			rec.Phase = "reviewing"
		}
		records[target.Key()] = rec
	}
	plan := PlanRetrySkipped(repo, pr, info.Title, RetrySkippedInput{
		RequestedHead: head,
		CurrentHead:   info.HeadSHA,
		State:         state,
		Draft:         info.Draft,
		IncludeDrafts: cfg.Poller.IncludeDrafts,
		Labels:        info.Labels,
		Rules:         cfg.Review.Labels,
		Records:       records,
		MaxDiffBytes:  cfg.Review.MaxDiffBytes,
		RepoAllowed:   true,
	})
	if len(plan.Result.Targets) == 0 {
		return plan.Result, nil
	}
	for i := range plan.Result.Targets {
		if plan.Result.Targets[i].Status != StatusAccepted {
			continue
		}
		var match *ReviewTarget
		for j := range plan.Start {
			if plan.Start[j].Key() == plan.Result.Targets[i].WorkflowID {
				match = &plan.Start[j]
				break
			}
		}
		if match == nil {
			continue
		}
		if err := src.StartTargetReview(ctx, *match); err != nil {
			plan.Result.Targets[i].Status = StatusError
			plan.Result.Targets[i].Detail = err.Error()
		}
	}
	plan.Result.Status = overallStatus(plan.Result.Targets)
	plan.Result.Detail = overallDetail(plan.Result.Targets)
	return plan.Result, nil
}

// workflowRunning reports whether the latest run of workflowID is running.
func (a *Activities) workflowRunning(ctx context.Context, workflowID string) (bool, error) {
	if a.Temporal == nil {
		return false, fmt.Errorf("temporal client is not configured")
	}
	resp, err := a.Temporal.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, err
	}
	info := resp.GetWorkflowExecutionInfo()
	if info == nil {
		return false, nil
	}
	return info.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, nil
}

// startTargetReview signal-with-starts one review. The caller must already
// have decided the target is a finished budget skip whose budget increased.
func (a *Activities) startTargetReview(ctx context.Context, t ReviewTarget) error {
	if a.Temporal == nil {
		return fmt.Errorf("temporal client is not configured")
	}
	return StartReview(ctx, a.Temporal, a.TaskQueue, t.Key(), reviewRequestForTarget(a.apiBase(), t))
}

func reviewRequestForTarget(apiBase string, t ReviewTarget) ReviewRequest {
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	return ReviewRequest{
		Repo: t.Repo, PRNumber: t.PRNumber, Title: t.Title, HeadSHA: t.HeadSHA,
		DiffURL:  fmt.Sprintf("%s/repos/%s/pulls/%d", strings.TrimRight(apiBase, "/"), t.Repo, t.PRNumber),
		LabelKey: t.LabelKey, Label: t.Label, Model: t.Model,
	}
}
