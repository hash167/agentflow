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

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
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
func (a *Activities) FetchDiff(ctx context.Context, request ReviewRequest) (string, error) {
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
	if response.StatusCode != http.StatusOK {
		err := fmt.Errorf("fetching diff from %s: HTTP %d", request.DiffURL, response.StatusCode)
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
	return string(body), nil
}

type reviewerInput struct {
	Repo     string `json:"repo"`
	PRNumber int    `json:"prNumber"`
	Title    string `json:"title"`
	Diff     string `json:"diff"`
}

// RunReviewAgent executes the configured reviewer command (the headless agent
// CLI in production, the bundled mock here) and parses its JSON verdict.
func (a *Activities) RunReviewAgent(ctx context.Context, request ReviewRequest) (ReviewResult, error) {
	result := ReviewResult{
		Repo:     request.Repo,
		PRNumber: request.PRNumber,
		HeadSHA:  request.HeadSHA,
		Findings: []ReviewFinding{},
	}

	if len(request.Diff) > a.Review.MaxDiffBytes {
		result.Skipped = fmt.Sprintf("diff is %d bytes, over the %d byte budget", len(request.Diff), a.Review.MaxDiffBytes)
		result.Summary = "Review skipped: " + result.Skipped
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
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				activity.RecordHeartbeat(ctx, "reviewing")
			}
		}
	}()

	input, err := json.Marshal(reviewerInput{
		Repo:     request.Repo,
		PRNumber: request.PRNumber,
		Title:    request.Title,
		Diff:     request.Diff,
	})
	if err != nil {
		return result, err
	}

	cmd := exec.CommandContext(ctx, a.Review.ReviewerCommand[0], a.Review.ReviewerCommand[1:]...)
	cmd.Stdin = bytes.NewReader(input)
	if a.Review.ReviewerTimeout != "" {
		// Config wins over the pod env so one file controls both timeouts.
		cmd.Env = append(os.Environ(), "CURSOR_REVIEW_TIMEOUT="+a.Review.ReviewerTimeout)
	}
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &tailBuffer{buf: &stderr, max: 2048})
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && ctx.Err() == nil {
			switch exitErr.ExitCode() {
			case ReviewerExitBadInput, ReviewerExitInvalidOutput:
				// Permanent: retrying the same diff will not fix the input or
				// the reviewer's configuration, so fail fast.
				return result, temporal.NewNonRetryableApplicationError(
					fmt.Sprintf("reviewer exited %d: %s", exitErr.ExitCode(), strings.TrimSpace(stderr.String())),
					"ReviewerPermanentError", err)
			}
		}
		return result, fmt.Errorf("reviewer command failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	var verdict struct {
		Summary  string          `json:"summary"`
		Findings []ReviewFinding `json:"findings"`
	}
	if err := json.Unmarshal(output, &verdict); err != nil {
		return result, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("reviewer produced invalid JSON: %v (output: %.200s)", err, output),
			"ReviewerInvalidJSON", err)
	}
	result.Summary = verdict.Summary
	if verdict.Findings != nil {
		result.Findings = verdict.Findings
	}
	return result, nil
}

// PostReview delivers the result to the configured sink.
func (a *Activities) PostReview(ctx context.Context, result ReviewResult) error {
	switch a.Review.Sink.Type {
	case "file":
		return a.postToFile(result)
	case "github":
		return a.postToGitHub(ctx, result)
	default:
		return fmt.Errorf("unknown sink type %q", a.Review.Sink.Type)
	}
}

func (a *Activities) postToFile(result ReviewResult) error {
	if err := os.MkdirAll(a.Review.Sink.Dir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-pr%d.json", strings.ReplaceAll(result.Repo, "/", "_"), result.PRNumber)
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
	url, created, err := gh.UpsertReviewComment(ctx, result.Repo, result.PRNumber, formatComment(result))
	if err != nil {
		return asTemporalError(err)
	}
	action := "updated"
	if created {
		action = "created"
	}
	log.Printf("[orchestrator] review comment %s: %s#%d %s", action, result.Repo, result.PRNumber, url)
	return nil
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
