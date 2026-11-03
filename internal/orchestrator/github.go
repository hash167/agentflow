package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"

	"go.temporal.io/sdk/temporal"
)

// ReviewMarker identifies the orchestrator's single PR comment so retries and
// re-reviews update it in place instead of posting duplicates.
const ReviewMarker = "<!-- agentflow-review -->"

var headMarkerRe = regexp.MustCompile(`<!-- agentflow-head: ([0-9a-fA-F]{7,40}) -->`)

// GitHubError is an HTTP error from the GitHub API.
type GitHubError struct {
	Status      int
	Op          string
	Body        string
	RateLimited bool
}

func (e *GitHubError) Error() string {
	return fmt.Sprintf("github %s: HTTP %d: %.300s", e.Op, e.Status, e.Body)
}

// Permanent reports whether retrying cannot help (bad token, no access,
// missing PR, invalid payload). Rate-limit 403/429 are retryable.
func (e *GitHubError) Permanent() bool {
	if e.RateLimited {
		return false
	}
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// asTemporalError wraps permanent GitHub failures as non-retryable.
func asTemporalError(err error) error {
	if ghErr, ok := err.(*GitHubError); ok && ghErr.Permanent() {
		return temporal.NewNonRetryableApplicationError(ghErr.Error(), "GitHubPermanentError", ghErr)
	}
	return err
}

// GitHubClient is a minimal REST client with ETag caching for GETs.
type GitHubClient struct {
	APIBase string
	Token   string
	HTTP    *http.Client

	mu    sync.Mutex
	etags map[string]cachedResponse
	// RateRemaining is the last seen X-RateLimit-Remaining (-1 if unknown).
	RateRemaining int
}

type cachedResponse struct {
	etag string
	body []byte
	link string
}

func NewGitHubClient(apiBase, token string) *GitHubClient {
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	return &GitHubClient{APIBase: strings.TrimRight(apiBase, "/"), Token: token, HTTP: http.DefaultClient, etags: map[string]cachedResponse{}, RateRemaining: -1}
}

func githubTokenFromEnv() (string, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return "", temporal.NewNonRetryableApplicationError("the github sink/poller requires the GITHUB_TOKEN environment variable", "MissingGitHubToken", nil)
	}
	return token, nil
}

func (c *GitHubClient) url(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return c.APIBase + path
}

// do sends a request; GETs use conditional requests (304s don't count
// against the rate limit). Returns body and the Link header.
func (c *GitHubClient) do(ctx context.Context, method, path, accept string, payload any, op string) ([]byte, string, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, "", err
		}
		body = bytes.NewReader(raw)
	}
	full := c.url(path)
	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	cacheKey := accept + " " + full
	var cached cachedResponse
	var haveCached bool
	if method == http.MethodGet {
		c.mu.Lock()
		cached, haveCached = c.etags[cacheKey]
		c.mu.Unlock()
		if haveCached {
			req.Header.Set("If-None-Match", cached.etag)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining != "" {
		var n int
		if _, err := fmt.Sscanf(remaining, "%d", &n); err == nil {
			c.mu.Lock()
			c.RateRemaining = n
			c.mu.Unlock()
		}
	}
	if resp.StatusCode == http.StatusNotModified && haveCached {
		return cached.body, cached.link, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 300 {
		rateLimited := resp.StatusCode == http.StatusTooManyRequests ||
			resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""
		return nil, "", &GitHubError{Status: resp.StatusCode, Op: op, Body: string(data), RateLimited: rateLimited}
	}
	link := resp.Header.Get("Link")
	if method == http.MethodGet {
		if etag := resp.Header.Get("ETag"); etag != "" {
			c.mu.Lock()
			c.etags[cacheKey] = cachedResponse{etag: etag, body: data, link: link}
			c.mu.Unlock()
		}
	}
	return data, link, nil
}

var nextLinkRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// getAll follows Link rel="next" pagination and decodes each page into T.
func getAll[T any](ctx context.Context, c *GitHubClient, path, op string) ([]T, error) {
	var all []T
	next := path
	for page := 0; next != "" && page < 50; page++ {
		data, link, err := c.do(ctx, http.MethodGet, next, "", nil, op)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, fmt.Errorf("github %s: decoding page: %w", op, err)
		}
		all = append(all, items...)
		next = ""
		if m := nextLinkRe.FindStringSubmatch(link); m != nil {
			next = m[1]
		}
	}
	return all, nil
}

type issueComment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

// FindReviewComment returns the comment carrying marker on a PR, if any.
// Markers end in " -->", so the default marker never matches a keyed one.
func (c *GitHubClient) FindReviewComment(ctx context.Context, repo string, pr int, marker string) (*issueComment, error) {
	comments, err := getAll[issueComment](ctx, c, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, pr), "list comments")
	if err != nil {
		return nil, err
	}
	for i := range comments {
		if strings.Contains(comments[i].Body, marker) {
			return &comments[i], nil
		}
	}
	return nil, nil
}

// UpsertReviewComment updates our marker comment (PATCH) or creates it
// (POST). Returns the comment URL and whether it was created.
func (c *GitHubClient) UpsertReviewComment(ctx context.Context, repo string, pr int, marker, body string) (string, bool, error) {
	existing, err := c.FindReviewComment(ctx, repo, pr, marker)
	if err != nil {
		return "", false, err
	}
	payload := map[string]string{"body": body}
	var data []byte
	created := existing == nil
	if existing != nil {
		data, _, err = c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, existing.ID), "", payload, "update comment")
	} else {
		data, _, err = c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, pr), "", payload, "create comment")
	}
	if err != nil {
		return "", false, err
	}
	var out issueComment
	_ = json.Unmarshal(data, &out)
	return out.HTMLURL, created, nil
}

// HeadSHAFromComment extracts the reviewed head SHA from a marker comment.
func HeadSHAFromComment(body string) string {
	if m := headMarkerRe.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func formatComment(result ReviewResult) string {
	var b strings.Builder
	b.WriteString(MarkerFor(result.LabelKey) + "\n")
	if result.HeadSHA != "" {
		fmt.Fprintf(&b, "<!-- agentflow-head: %s -->\n", result.HeadSHA)
	}
	if result.Model != "" {
		fmt.Fprintf(&b, "<!-- agentflow-model: %s -->\n", result.Model)
		fmt.Fprintf(&b, "## Agent code review (`%s`)\n\n", result.Model)
		if result.Label != "" {
			fmt.Fprintf(&b, "Model `%s`, requested by label `%s`.\n\n", result.Model, result.Label)
		} else {
			fmt.Fprintf(&b, "Model `%s`.\n\n", result.Model)
		}
	} else {
		b.WriteString("## Agent code review\n\n")
	}
	if result.HeadSHA != "" {
		short := result.HeadSHA
		if len(short) > 12 {
			short = short[:12]
		}
		fmt.Fprintf(&b, "Reviewed commit `%s`.\n\n", short)
	}
	b.WriteString(result.Summary)
	b.WriteString("\n")
	for _, finding := range result.Findings {
		location := finding.File
		if finding.Line > 0 {
			location = fmt.Sprintf("%s:%d", finding.File, finding.Line)
		}
		fmt.Fprintf(&b, "\n- **%s** `%s` — %s", finding.Severity, location, finding.Message)
	}
	if len(result.Findings) == 0 && result.Skipped == "" {
		b.WriteString("\nNo findings.")
	}
	b.WriteString("\n\n<sub>Updated in place by AgentFlow on each new push.</sub>\n")
	return b.String()
}
