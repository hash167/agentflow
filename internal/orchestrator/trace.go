package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/activity"

	"agent-gateway/internal/telemetry"
)

func corr(req ReviewRequest) telemetry.Correlation {
	key := req.LabelKey
	if key == "" {
		key = "default"
	}
	return telemetry.Correlation{
		Repo:      req.Repo,
		Forge:     req.Forge,
		PRNumber:  req.PRNumber,
		HeadSHA:   req.HeadSHA,
		LabelKey:  req.LabelKey,
		Label:     req.Label,
		Model:     req.Model,
		ReviewKey: ReviewWorkflowID(req.Repo, req.PRNumber, key),
		RunKind:   req.RunKind,
	}
}

func corrTarget(t ReviewTarget) telemetry.Correlation {
	return corr(ReviewRequest{
		Repo: t.Repo, PRNumber: t.PRNumber, HeadSHA: t.HeadSHA,
		LabelKey: t.LabelKey, Label: t.Label, Model: t.Model,
	})
}

func activityCorr(ctx context.Context, base telemetry.Correlation) telemetry.Correlation {
	info := activity.GetInfo(ctx)
	base.Workflow = info.WorkflowExecution.ID
	base.RunID = info.WorkflowExecution.RunID
	return base
}

func attemptAttrs(ctx context.Context) []attribute.KeyValue {
	info := activity.GetInfo(ctx)
	return []attribute.KeyValue{
		attribute.Int(telemetry.AttrTemporalAttempt, int(info.Attempt)),
		attribute.String(telemetry.AttrTemporalActType, info.ActivityType.Name),
		attribute.String(telemetry.AttrTemporalWorkflow, info.WorkflowExecution.ID),
		attribute.String(telemetry.AttrTemporalRunID, info.WorkflowExecution.RunID),
	}
}

func startSpan(ctx context.Context, name string, c telemetry.Correlation, extra ...attribute.KeyValue) (context.Context, trace.Span) {
	attrs := append(c.Attrs(), extra...)
	attrs = append(attrs, attemptAttrs(ctx)...)
	return telemetry.Start(ctx, name, attrs...)
}

func startSpanLinked(ctx context.Context, name, traceLink string, c telemetry.Correlation, extra ...attribute.KeyValue) (context.Context, trace.Span) {
	attrs := append(c.Attrs(), extra...)
	attrs = append(attrs, attemptAttrs(ctx)...)
	if l, ok := telemetry.LinkFromTraceParent(traceLink); ok {
		return telemetry.StartLinked(ctx, name, l, attrs...)
	}
	return telemetry.Start(ctx, name, attrs...)
}

func (r ReviewConfig) Hash() string {
	payload, err := json.Marshal(struct {
		Command         []string `json:"command"`
		MaxDiffBytes    int      `json:"maxDiffBytes"`
		ReviewerTimeout string   `json:"reviewerTimeout"`
		ActivityTimeout string   `json:"activityTimeout"`
		Sink            string   `json:"sink"`
		Labels          int      `json:"labels"`
	}{r.ReviewerCommand, r.MaxDiffBytes, r.ReviewerTimeout, r.ActivityTimeout, r.Sink.Type, len(r.Labels)})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:8])
}

func diffSHA256(diff string) string {
	sum := sha256.Sum256([]byte(diff))
	return hex.EncodeToString(sum[:])
}

func diffFileCount(diff string) int {
	n := 0
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "diff --git ") {
			n++
		}
	}
	if n == 0 && strings.TrimSpace(diff) != "" {
		return 1
	}
	// Prefer +++ counts when both match-style markers exist.
	plus := 0
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ ") {
			plus++
		}
	}
	if plus > 0 {
		return plus
	}
	return n
}

func previousAttemptOutcome(ctx context.Context) string {
	if activity.GetInfo(ctx).Attempt > 1 {
		return telemetry.OutcomeUndetermined
	}
	return ""
}
