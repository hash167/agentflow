package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
)

type Activities struct {
	Review ReviewConfig
}

// FetchDiff downloads the unified diff for a review request.
func (a *Activities) FetchDiff(ctx context.Context, request ReviewRequest) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, request.DiffURL, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching diff from %s: HTTP %d", request.DiffURL, response.StatusCode)
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
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	if err != nil {
		return result, fmt.Errorf("reviewer command failed: %w", err)
	}

	var verdict struct {
		Summary  string          `json:"summary"`
		Findings []ReviewFinding `json:"findings"`
	}
	if err := json.Unmarshal(output, &verdict); err != nil {
		return result, fmt.Errorf("reviewer produced invalid JSON: %w (output: %.200s)", err, output)
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

func formatComment(result ReviewResult) string {
	var b strings.Builder
	b.WriteString("## Agent code review\n\n")
	b.WriteString(result.Summary)
	b.WriteString("\n")
	for _, finding := range result.Findings {
		location := finding.File
		if finding.Line > 0 {
			location = fmt.Sprintf("%s:%d", finding.File, finding.Line)
		}
		fmt.Fprintf(&b, "\n- **%s** `%s` — %s", finding.Severity, location, finding.Message)
	}
	return b.String()
}

func (a *Activities) postToGitHub(ctx context.Context, result ReviewResult) error {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return fmt.Errorf("github sink requires the GITHUB_TOKEN environment variable")
	}
	apiBase := a.Review.Sink.APIBase
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/repos/%s/issues/%d/comments", strings.TrimRight(apiBase, "/"), result.Repo, result.PRNumber)

	payload, err := json.Marshal(map[string]string{"body": formatComment(result)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("posting review comment: HTTP %d: %s", response.StatusCode, body)
	}
	return nil
}
