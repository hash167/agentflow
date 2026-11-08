package telemetry

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"

	"go.temporal.io/sdk/temporal"
)

// Error classes (agentflow.error.class). Keep the set small and stable.
const (
	ClassRateLimit       = "rate_limit"
	ClassUsageLimit      = "usage_limit"
	ClassTimeoutReviewer = "timeout.reviewer"
	ClassTimeoutActivity = "timeout.activity"
	ClassKilled          = "killed"
	ClassCLIError        = "cli_error"
	ClassParseEnvelope   = "parse.envelope"
	ClassParseVerdict    = "parse.verdict"
	ClassBadInput        = "bad_input"
	ClassDiffFetch       = "diff_fetch"
	ClassPostGitHub      = "post.github"
	ClassPostOrigin      = "post.origin"
	ClassGatewayAuth     = "gateway.auth"
	ClassGatewayPolicy   = "gateway.policy_denied"
	ClassGatewayUpstream = "gateway.upstream"
)

var usageLimitNeedles = []string{
	"usage limit", "usage-limit", "usagelimit",
	"quota exceeded", "quota limit", "out of credits",
	"credit limit", "plan limit", "rate limit reached",
	"resource exhausted", "resource_exhausted",
}

var rateLimitNeedles = []string{
	"rate limit", "ratelimit", "too many requests", "status 429", "http 429",
}

// Classify maps an error (and optional stderr) onto the taxonomy. It never
// invents token counts or model-call details.
func Classify(err error, stderr string) string {
	if err == nil {
		return ""
	}
	blob := strings.ToLower(err.Error() + "\n" + stderr)

	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		switch app.Type() {
		case "DiffFetchPermanent":
			return ClassDiffFetch
		case "ReviewerPermanentError":
			if strings.Contains(blob, "exited 2") {
				return ClassBadInput
			}
			if strings.Contains(blob, "exited 3") {
				return ClassParseVerdict
			}
			return ClassCLIError
		case "ReviewerInvalidJSON":
			return ClassParseEnvelope
		case "GitHubPermanentError", "MissingGitHubToken":
			return ClassPostGitHub
		}
		if app.Type() == "ActivityError" || strings.Contains(strings.ToLower(app.Type()), "timeout") {
			return ClassTimeoutActivity
		}
	}

	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(blob, "deadline exceeded") {
		if strings.Contains(blob, "starttoclose") || strings.Contains(blob, "heartbeat") {
			return ClassTimeoutActivity
		}
		return ClassTimeoutReviewer
	}
	if errors.Is(err, context.Canceled) {
		return ClassKilled
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return ClassKilled
		}
		switch exitErr.ExitCode() {
		case 2:
			return ClassBadInput
		case 3:
			if strings.Contains(blob, "envelope") {
				return ClassParseEnvelope
			}
			return ClassParseVerdict
		}
	}

	if looksLike(blob, usageLimitNeedles) {
		return ClassUsageLimit
	}
	if looksLike(blob, rateLimitNeedles) || strings.Contains(blob, "http 429") {
		return ClassRateLimit
	}
	if strings.Contains(blob, "fetching diff") || strings.Contains(blob, "difffetch") {
		return ClassDiffFetch
	}
	if strings.Contains(blob, "github ") || strings.Contains(blob, "posting review comment") || strings.Contains(blob, "update comment") || strings.Contains(blob, "create comment") {
		return ClassPostGitHub
	}
	if strings.Contains(blob, "invalid json") || strings.Contains(blob, "json result envelope") {
		return ClassParseEnvelope
	}
	if strings.Contains(blob, "invalid reviewer output") {
		return ClassParseVerdict
	}
	if errors.Is(err, io.EOF) {
		return ClassCLIError
	}
	return ClassCLIError
}

func looksLike(blob string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(blob, n) {
			return true
		}
	}
	return false
}

func OutcomeFor(err error, stderr string, skipped string, findings int, posted bool) string {
	if err != nil {
		switch Classify(err, stderr) {
		case ClassUsageLimit:
			return OutcomeUsageLimited
		case ClassTimeoutReviewer, ClassTimeoutActivity:
			return OutcomeTimedOut
		default:
			return OutcomeFailed
		}
	}
	if skipped != "" {
		return OutcomeSkipped
	}
	if !posted {
		return OutcomeUndetermined
	}
	if findings == 0 {
		return OutcomeReviewedClean
	}
	return OutcomeReviewed
}
