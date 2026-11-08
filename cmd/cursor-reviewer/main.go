// cursor-reviewer is a review.reviewerCommand that runs the headless Cursor
// CLI (`agent --print --output-format json`) against the PR diff and prints a
// validated {"summary","findings"} verdict. Configuration (env):
//
//	CURSOR_API_KEY          required, used by the Cursor CLI
//	CURSOR_REVIEW_MODEL     default grok-4.7
//	CURSOR_REVIEW_TIMEOUT   default 12m (below the activity's 15m StartToClose)
//	CURSOR_AGENT_BIN        default "agent"
//	AGENTFLOW_GATEWAY_URL   MCP gateway URL (default in-cluster service; "-" disables)
//	GATEWAY_REVIEWER_KEY    gateway bearer key (referenced via ${env:...}, never logged)
//	CURSOR_REVIEW_MODE      optional --mode (ask|plan)
//	OTEL_EXPORTER_OTLP_ENDPOINT  optional; empty keeps tracing a no-op
//
// Exit codes: 0 ok, 1 agent failure (retryable), 2 bad input/config and
// 3 invalid verdict JSON (both permanent).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent-gateway/internal/reviewer"
	"agent-gateway/internal/telemetry"
)

func main() {
	tel := telemetry.Init(context.Background(), telemetry.Options{ServiceName: "cursor-reviewer"})
	defer tel.Shutdown(2 * time.Second)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := reviewer.Run(ctx, reviewer.FromEnv(), os.Stdin, os.Stdout, os.Stderr)
	tel.ForceFlush(2 * time.Second)
	os.Exit(code)
}
