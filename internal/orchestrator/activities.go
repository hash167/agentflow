package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"agent-gateway/internal/reviewer"
	"agent-gateway/internal/telemetry"
)

type Activities struct {
	Review ReviewConfig
	Poller PollerConfig
	// Temporal is used by the poller activities to read per-PR workflow
	// state and to signal-with-start reviews. Optional otherwise.
	Temporal  client.Client
	TaskQueue string

	ghOnce sync.Mutex
	gh     *GitHubClient
}

// github returns a shared client (shared so ETags persist across poll runs).
func (a *Activities) github() (*GitHubClient, error) {
	token, err := githubTokenFromEnv()
	if err != nil {
		return nil, err
	}
	a.ghOnce.Lock()
	defer a.ghOnce.Unlock()
	if a.gh == nil || a.gh.Token != token {
		a.gh = NewGitHubClient(a.Review.Sink.APIBase, token)
	}
	return a.gh, nil
}

func (a *Activities) apiBase() string {
	if a.Review.Sink.APIBase != "" {
		return strings.TrimRight(a.Review.Sink.APIBase, "/")
	}
	return "https://api.github.com"
}

// FetchDiff downloads the unified diff for a review request.
func (a *Activities) FetchDiff(ctx context.Context, request ReviewRequest) (diff string, err error) {
	c := activityCorr(ctx, corr(request))
	telemetry.RecordAttemptStarted(ctx, "FetchDiff", c, attemptAttrs(ctx)...)
	ctx, span := startSpanLinked(ctx, telemetry.SpanDiffFetch, request.TraceLink, c)
	defer func() {
		if err != nil {
			telemetry.End(span, err, "")
			telemetry.RecordAttemptFinished(ctx, "FetchDiff", telemetry.OutcomeFailed, telemetry.Classify(err, ""), c, attemptAttrs(ctx)...)
			return
		}
		telemetry.End(span, nil, "")
		telemetry.RecordAttemptFinished(ctx, "FetchDiff", "", "", c, attemptAttrs(ctx)...)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, request.DiffURL, nil)
	if err != nil {
		return "", err
	}
	// GitHub API PR URLs need the token and the diff media type.
	if strings.HasPrefix(request.DiffURL, a.apiBase()+"/") {
		if token := os.Getenv("GITHUB_TOKEN"); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Accept", "application/vnd.github.diff")
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	span.SetAttributes(attribute.Int(telemetry.AttrHTTPStatus, response.StatusCode))
	if response.StatusCode != http.StatusOK {
		err := fmt.Errorf("fetching diff: HTTP %d", response.StatusCode)
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusNotFound:
			return "", temporal.NewNonRetryableApplicationError(err.Error(), "DiffFetchPermanent", nil)
		}
		return "", err
	}
	limit := int64(a.Review.MaxDiffBytes) + 1
	body, err := io.ReadAll(io.LimitReader(response.Body, limit))
	if err != nil {
		return "", err
	}
	truncated := int64(len(body)) > int64(a.Review.MaxDiffBytes)
	span.SetAttributes(
		attribute.Int(telemetry.AttrDiffBytes, len(body)),
		attribute.Int(telemetry.AttrDiffFiles, diffFileCount(string(body))),
		attribute.String(telemetry.AttrDiffSHA256, diffSHA256(string(body))),
		attribute.Bool(telemetry.AttrDiffTruncated, truncated),
		attribute.Bool(telemetry.AttrDiffOverBudget, truncated),
	)
	return string(body), nil
}

type reviewerInput struct {
	Repo     string `json:"repo"`
	PRNumber int    `json:"prNumber"`
	Title    string `json:"title"`
	Diff     string `json:"diff"`
	Model    string `json:"model,omitempty"`
}

// RunReviewAgent executes the configured reviewer command (the headless agent
// CLI in production, the bundled mock here) and parses its JSON verdict.
func (a *Activities) RunReviewAgent(ctx context.Context, request ReviewRequest) (result ReviewResult, err error) {
	result = ReviewResult{
		Repo:     request.Repo,
		PRNumber: request.PRNumber,
		HeadSHA:  request.HeadSHA,
		Findings: []ReviewFinding{},
		LabelKey: request.LabelKey,
		Label:    request.Label,
		Model:    request.Model,
	}
	c := activityCorr(ctx, corr(request))
	attemptExtra := attemptAttrs(ctx)
	if prev := previousAttemptOutcome(ctx); prev != "" {
		attemptExtra = append(attemptExtra, attribute.String(telemetry.AttrPreviousOutcome, prev))
	}
	telemetry.RecordAttemptStarted(ctx, "RunReviewAgent", c, attemptExtra...)

	timeout, _, _ := a.Review.Timeouts()
	ctx, invoke := startSpanLinked(ctx, telemetry.SpanInvokeAgent, request.TraceLink, c,
		attribute.String(telemetry.AttrGenAIOp, "invoke_agent"),
		attribute.String(telemetry.AttrGenAIProvider, telemetry.ProviderCursor),
		attribute.String(telemetry.AttrPromptHash, reviewer.PromptTemplateHash()),
		attribute.String(telemetry.AttrPromptVersion, reviewer.PromptTemplateVersion),
		attribute.String(telemetry.AttrReviewerVersion, telemetry.ServiceVersion),
		attribute.String(telemetry.AttrConfigHash, a.Review.Hash()),
		attribute.Int(telemetry.AttrReviewerTimeoutS, int(timeout.Seconds())),
		attribute.Int(telemetry.AttrDiffBytes, len(request.Diff)),
		attribute.String(telemetry.AttrDiffSHA256, diffSHA256(request.Diff)),
	)
	var stderrTail string
	defer func() {
		outcome := telemetry.OutcomeFor(err, stderrTail, result.Skipped, len(result.Findings), err == nil)
		telemetry.SetOutcome(invoke, outcome, telemetry.Classify(err, stderrTail), result.Skipped)
		telemetry.End(invoke, err, stderrTail)
		telemetry.RecordAttemptFinished(ctx, "RunReviewAgent", outcome, telemetry.Classify(err, stderrTail), c, attemptExtra...)
	}()

	if len(request.Diff) > a.Review.MaxDiffBytes {
		result.Skipped = fmt.Sprintf("diff is %d bytes, over the %d byte budget", len(request.Diff), a.Review.MaxDiffBytes)
		result.Summary = "Review skipped: " + result.Skipped
		invoke.SetAttributes(attribute.Bool(telemetry.AttrDiffOverBudget, true))
		return result, nil
	}
	if strings.TrimSpace(request.Diff) == "" {
		result.Skipped = "empty diff"
		result.Summary = "Review skipped: empty diff"
		return result, nil
	}

	// Heartbeat while the agent runs so Temporal can distinguish a slow
	// review from a dead worker.
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	startedAt := time.Now()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		n := 0
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				n++
				activity.RecordHeartbeat(ctx, "reviewing")
				telemetry.HeartbeatEvent(invoke, n, startedAt)
			}
		}
	}()

	_, prep := startSpan(ctx, telemetry.SpanReviewerPrepare, c)
	input, err := json.Marshal(reviewerInput{
		Repo:     request.Repo,
		PRNumber: request.PRNumber,
		Title:    request.Title,
		Diff:     request.Diff,
		Model:    request.Model,
	})
	telemetry.End(prep, err, "")
	if err != nil {
		return result, err
	}

	ctx, sub := startSpan(ctx, telemetry.SpanReviewerSubproc, c)
	cmd := exec.CommandContext(ctx, a.Review.ReviewerCommand[0], a.Review.ReviewerCommand[1:]...)
	cmd.Stdin = bytes.NewReader(input)
	parentEnv := os.Environ()
	if a.Review.ReviewerTimeout != "" {
		parentEnv = append(parentEnv, "CURSOR_REVIEW_TIMEOUT="+a.Review.ReviewerTimeout)
	}
	if request.Model != "" {
		parentEnv = append(parentEnv, "CURSOR_REVIEW_MODEL="+request.Model)
	}
	cmd.Env = telemetry.ChildEnv(parentEnv, ctx, "cursor-reviewer")
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &tailBuffer{buf: &stderr, max: 2048})
	output, err := cmd.Output()
	stderrTail = stderr.String()
	if cmd.Process != nil {
		sub.SetAttributes(attribute.Int(telemetry.AttrProcessPID, cmd.Process.Pid))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		sub.SetAttributes(attribute.Int(telemetry.AttrProcessExit, exitErr.ExitCode()))
		class := "agent_failed"
		switch exitErr.ExitCode() {
		case ReviewerExitBadInput:
			class = "bad_input"
		case ReviewerExitInvalidOutput:
			class = "invalid_output"
		}
		if ctx.Err() != nil {
			class = "killed"
		}
		sub.SetAttributes(
			attribute.String(telemetry.AttrReviewerExitClass, class),
			attribute.String(telemetry.AttrReviewerStderr, telemetry.SafeStderr(stderrTail)),
		)
	} else if err == nil {
		sub.SetAttributes(attribute.String(telemetry.AttrReviewerExitClass, "ok"), attribute.Int(telemetry.AttrProcessExit, 0))
	}
	telemetry.End(sub, err, stderrTail)
	if err != nil {
		if errors.As(err, &exitErr) && ctx.Err() == nil {
			switch exitErr.ExitCode() {
			case ReviewerExitBadInput, ReviewerExitInvalidOutput:
				// Permanent: retrying the same diff will not fix the input or
				// the reviewer's configuration, so fail fast.
				return result, temporal.NewNonRetryableApplicationError(
					fmt.Sprintf("reviewer exited %d: %s", exitErr.ExitCode(), telemetry.SafeStderr(strings.TrimSpace(stderrTail))),
					"ReviewerPermanentError", err)
			}
		}
		return result, fmt.Errorf("reviewer command failed: %w (stderr: %s)", err, telemetry.SafeStderr(strings.TrimSpace(stderrTail)))
	}

	_, parse := startSpan(ctx, telemetry.SpanReviewerParse, c, attribute.String(telemetry.AttrParseStage, "envelope"))
	var verdict struct {
		Summary  string          `json:"summary"`
		Findings []ReviewFinding `json:"findings"`
	}
	if err := json.Unmarshal(output, &verdict); err != nil {
		parse.SetAttributes(attribute.String(telemetry.AttrParseStage, "envelope"))
		telemetry.End(parse, err, "")
		return result, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("reviewer produced invalid JSON: %v", err),
			"ReviewerInvalidJSON", err)
	}
	result.Summary = verdict.Summary
	if verdict.Findings != nil {
		result.Findings = verdict.Findings
	}
	parse.SetAttributes(
		attribute.String(telemetry.AttrParseStage, "validate"),
		attribute.Int(telemetry.AttrFindingsCount, len(result.Findings)),
	)
	for _, f := range result.Findings {
		parse.AddEvent("agentflow.finding", trace.WithAttributes(
			attribute.String("agentflow.finding.file", f.File),
			attribute.Int("agentflow.finding.line", f.Line),
			attribute.String("agentflow.finding.severity", f.Severity),
		))
	}
	telemetry.End(parse, nil, "")
	return result, nil
}

func (a *Activities) postToFile(result ReviewResult) error {
	if err := os.MkdirAll(a.Review.Sink.Dir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-pr%d.json", strings.ReplaceAll(result.Repo, "/", "_"), result.PRNumber)
	if result.LabelKey != "" {
		name = fmt.Sprintf("%s-pr%d-%s.json", strings.ReplaceAll(result.Repo, "/", "_"), result.PRNumber, result.LabelKey)
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(a.Review.Sink.Dir, name), append(payload, '\n'), 0o644)
}

func (a *Activities) postToGitHub(ctx context.Context, result ReviewResult) error {
	gh, err := a.github()
	if err != nil {
		return err
	}
	url, created, err := gh.UpsertReviewComment(ctx, result.Repo, result.PRNumber, MarkerFor(result.LabelKey), formatComment(result))
	if err != nil {
		return asTemporalError(err)
	}
	action := "updated"
	if created {
		action = "created"
	}
	log.Printf("[orchestrator] review comment %s: %s#%d [%s %s] %s", action, result.Repo, result.PRNumber, result.LabelKey, result.Model, url)
	return nil
}

func (a *Activities) PostReview(ctx context.Context, result ReviewResult) (err error) {
	req := ReviewRequest{Repo: result.Repo, PRNumber: result.PRNumber, HeadSHA: result.HeadSHA, LabelKey: result.LabelKey, Label: result.Label, Model: result.Model}
	c := activityCorr(ctx, corr(req))
	telemetry.RecordAttemptStarted(ctx, "PostReview", c, attemptAttrs(ctx)...)
	ctx, span := startSpan(ctx, telemetry.SpanCommentUpsert, c,
		attribute.String(telemetry.AttrSink, a.Review.Sink.Type),
		attribute.String(telemetry.AttrCommentMarker, MarkerFor(result.LabelKey)),
	)
	defer func() {
		outcome := telemetry.OutcomeFor(err, "", result.Skipped, len(result.Findings), err == nil)
		if err == nil {
			telemetry.RecordCycleFinished(ctx, c, outcome, "", attribute.Int(telemetry.AttrFindingsCount, len(result.Findings)))
		}
		telemetry.End(span, err, "")
		telemetry.RecordAttemptFinished(ctx, "PostReview", outcome, telemetry.Classify(err, ""), c, attemptAttrs(ctx)...)
	}()
	switch a.Review.Sink.Type {
	case "file":
		span.SetAttributes(attribute.String(telemetry.AttrCommentAction, "created"))
		return a.postToFile(result)
	case "github":
		e := a.postToGitHub(ctx, result)
		if e == nil {
			span.SetAttributes(attribute.String(telemetry.AttrCommentAction, "upserted"))
		}
		return e
	default:
		return fmt.Errorf("unknown sink type %q", a.Review.Sink.Type)
	}
}

// Reviewer exit codes with permanent meaning (see cmd/cursor-reviewer).
const (
	ReviewerExitBadInput      = 2
	ReviewerExitInvalidOutput = 3
)

// tailBuffer keeps at most max bytes (the latest) for error messages.
type tailBuffer struct {
	buf *bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if extra := t.buf.Len() - t.max; extra > 0 {
		t.buf.Next(extra)
	}
	return len(p), nil
}
