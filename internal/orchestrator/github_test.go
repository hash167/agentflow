package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
)

// fakeGitHub is an in-memory issue-comments API.
type fakeGitHub struct {
	mu       sync.Mutex
	comments []issueComment
	nextID   int64
	posts    int
	patches  int
	failWith int
	srv      *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{nextID: 100}
	// Pad with unrelated comments so the marker comment lands on page 2.
	for i := 0; i < 3; i++ {
		f.comments = append(f.comments, issueComment{ID: int64(i + 1), Body: "human comment"})
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		return
	}
	if f.failWith != 0 {
		w.WriteHeader(f.failWith)
		_, _ = io.WriteString(w, `{"message":"nope"}`)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/8/comments":
		page := r.URL.Query().Get("page")
		start, end := 0, 2
		if page == "2" {
			start, end = 2, len(f.comments)
		} else if len(f.comments) > 2 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/8/comments?per_page=2&page=2>; rel="next"`, f.srv.URL))
		}
		if end > len(f.comments) {
			end = len(f.comments)
		}
		_ = json.NewEncoder(w).Encode(f.comments[start:end])
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/8/comments":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.posts++
		f.nextID++
		c := issueComment{ID: f.nextID, Body: body["body"], HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/8#issuecomment-%d", f.nextID)}
		f.comments = append(f.comments, c)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(c)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/comments/"):
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.patches++
		for i := range f.comments {
			if fmt.Sprintf("/repos/o/r/issues/comments/%d", f.comments[i].ID) == r.URL.Path {
				f.comments[i].Body = body["body"]
				f.comments[i].HTMLURL = fmt.Sprintf("https://github.com/o/r/pull/8#issuecomment-%d", f.comments[i].ID)
				_ = json.NewEncoder(w).Encode(f.comments[i])
				return
			}
		}
		w.WriteHeader(404)
	default:
		w.WriteHeader(404)
	}
}

func githubActivities(t *testing.T, f *fakeGitHub) *Activities {
	t.Setenv("GITHUB_TOKEN", "tok")
	return &Activities{Review: ReviewConfig{Sink: SinkConfig{Type: "github", APIBase: f.srv.URL}}}
}

func TestPostReviewUpsertsSingleComment(t *testing.T) {
	f := newFakeGitHub(t)
	a := githubActivities(t, f)
	ctx := context.Background()

	first := ReviewResult{Repo: "o/r", PRNumber: 8, HeadSHA: "aaaaaaa1111111", Summary: "first"}
	require.NoError(t, a.postToGitHub(ctx, first))
	require.Equal(t, 1, f.posts)

	// Retry of the same result and a later push both update in place.
	require.NoError(t, a.postToGitHub(ctx, first))
	second := ReviewResult{Repo: "o/r", PRNumber: 8, HeadSHA: "bbbbbbb2222222", Summary: "second",
		Findings: []ReviewFinding{{File: "a.go", Line: 3, Severity: "error", Message: "bug"}}}
	require.NoError(t, a.postToGitHub(ctx, second))

	require.Equal(t, 1, f.posts, "must never post a second comment")
	require.Equal(t, 2, f.patches)
	marked := 0
	var body string
	for _, c := range f.comments {
		if strings.Contains(c.Body, ReviewMarker) {
			marked++
			body = c.Body
		}
	}
	require.Equal(t, 1, marked)
	require.Contains(t, body, "second")
	require.Contains(t, body, "`bbbbbbb22222`")
	require.Equal(t, "bbbbbbb2222222", HeadSHAFromComment(body))
}

func TestPostReviewPermanentErrorsAreNonRetryable(t *testing.T) {
	for _, status := range []int{401, 403, 404, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFakeGitHub(t)
			f.failWith = status
			a := githubActivities(t, f)
			err := a.postToGitHub(context.Background(), ReviewResult{Repo: "o/r", PRNumber: 8, Summary: "s"})
			var appErr *temporal.ApplicationError
			require.True(t, errors.As(err, &appErr), "got %v", err)
			require.True(t, appErr.NonRetryable())
		})
	}
}

func TestPostReviewTransientErrorsAreRetryable(t *testing.T) {
	for _, status := range []int{500, 502, 429} {
		f := newFakeGitHub(t)
		f.failWith = status
		a := githubActivities(t, f)
		err := a.postToGitHub(context.Background(), ReviewResult{Repo: "o/r", PRNumber: 8, Summary: "s"})
		require.Error(t, err)
		var appErr *temporal.ApplicationError
		require.False(t, errors.As(err, &appErr) && appErr.NonRetryable(), "status %d should be retryable", status)
	}
}

func TestRateLimited403IsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(403)
	}))
	defer srv.Close()
	gh := NewGitHubClient(srv.URL, "tok")
	_, err := gh.FindReviewComment(context.Background(), "o/r", 8)
	var ghErr *GitHubError
	require.True(t, errors.As(err, &ghErr))
	require.False(t, ghErr.Permanent())
	require.Equal(t, 0, gh.RateRemaining)
}

func TestConditionalGetUsesETag(t *testing.T) {
	var conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, `[{"number":1,"head":{"sha":"abc"}}]`)
	}))
	defer srv.Close()
	gh := NewGitHubClient(srv.URL, "tok")
	for i := 0; i < 2; i++ {
		pulls, err := getAll[ghPull](context.Background(), gh, "/repos/o/r/pulls?state=open", "list pulls")
		require.NoError(t, err)
		require.Len(t, pulls, 1)
		require.Equal(t, "abc", pulls[0].Head.SHA)
	}
	require.Equal(t, 1, conditional)
}
