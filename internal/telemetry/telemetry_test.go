package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	h := &Handle{tp: tp, tracer: tp.Tracer("test"), enabled: true}
	old := Install(h)
	t.Cleanup(func() {
		Install(old)
		_ = tp.Shutdown(context.Background())
	})
	return sr
}

func spanByName(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range sr.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("span %q not found in %v", name, names(sr))
	return nil
}

func names(sr *tracetest.SpanRecorder) []string {
	var out []string
	for _, s := range sr.Ended() {
		out = append(out, s.Name())
	}
	return out
}

func attrMap(s sdktrace.ReadOnlySpan) map[string]string {
	m := map[string]string{}
	for _, a := range s.Attributes() {
		m[string(a.Key)] = a.Value.Emit()
	}
	return m
}

func TestDisabledInitIsNoop(t *testing.T) {
	old := Current()
	t.Cleanup(func() { Install(old) })
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	h := Init(context.Background(), Options{ServiceName: "orchestrator"})
	require.False(t, h.Enabled())
	ctx, span := Start(context.Background(), "should-not-record")
	span.End()
	require.False(t, span.SpanContext().IsValid() && span.IsRecording())
	_ = ctx
}

func TestTracingEnvZeroForcesNoop(t *testing.T) {
	old := Current()
	t.Cleanup(func() { Install(old) })
	t.Setenv("AGENTFLOW_TRACING", "0")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	h := Init(context.Background(), Options{ServiceName: "orchestrator", Enabled: true, Endpoint: "http://127.0.0.1:1"})
	require.False(t, h.Enabled())
}

func TestSuccessAndErrorSpans(t *testing.T) {
	sr := installRecorder(t)
	corr := Correlation{Repo: "hash167/agentflow", PRNumber: 43, HeadSHA: "deadbeef", LabelKey: "default", Model: "grok-4.7", ReviewKey: "review:hash167/agentflow#43:default"}
	ctx, span := Start(context.Background(), SpanDiffFetch, corr.Attrs()...)
	span.SetAttributes()
	End(span, nil, "")
	_, fail := Start(ctx, SpanReviewerParse, corr.Attrs()...)
	End(fail, errors.New("CLI did not print a JSON result envelope"), "")
	require.Equal(t, SpanDiffFetch, spanByName(t, sr, SpanDiffFetch).Name())
	failed := spanByName(t, sr, SpanReviewerParse)
	require.Equal(t, ClassParseEnvelope, attrMap(failed)[AttrErrorClass])
}

func TestRetryEmitsPerAttemptMarkers(t *testing.T) {
	sr := installRecorder(t)
	corr := Correlation{Repo: "o/r", PRNumber: 1, ReviewKey: "review:o/r#1:default"}
	ctx := context.Background()
	RecordAttemptStarted(ctx, "RunReviewAgent", corr)
	RecordAttemptFinished(ctx, "RunReviewAgent", OutcomeFailed, ClassCLIError, corr)
	RecordAttemptStarted(ctx, "RunReviewAgent", corr)
	RecordAttemptFinished(ctx, "RunReviewAgent", OutcomeReviewedClean, "", corr)
	require.Equal(t, []string{SpanAttemptStarted, SpanAttemptFinished, SpanAttemptStarted, SpanAttemptFinished}, names(sr))
}

func TestContextRoundTripAndLinks(t *testing.T) {
	sr := installRecorder(t)
	ctx, parent := Start(context.Background(), SpanStartTarget, Correlation{Repo: "o/r", PRNumber: 1}.Attrs()...)
	parent.End()
	tp := TraceParent(ctx)
	require.NotEmpty(t, tp)
	require.True(t, strings.HasPrefix(tp, "00-"))

	env := InjectEnv(ctx)
	require.NotEmpty(t, env)
	extracted := ExtractEnv(context.Background(), append([]string{"PATH=/bin", "GITHUB_TOKEN=should-not-matter"}, env...))
	got := TraceParent(extracted)
	require.Equal(t, tp, got)

	link, ok := LinkFromTraceParent(tp)
	require.True(t, ok)
	cycleTP := RecordCycleStarted(context.Background(), Correlation{Repo: "o/r", PRNumber: 1, ReviewKey: "review:o/r#1:default"}, tp)
	require.NotEmpty(t, cycleTP)
	require.NotEqual(t, tp, cycleTP, "cycle is a new root trace")
	started := spanByName(t, sr, SpanCycleStarted)
	require.Len(t, started.Links(), 1)
	require.Equal(t, link.SpanContext.TraceID(), started.Links()[0].SpanContext.TraceID())
}

func TestWithoutSpanStripsParent(t *testing.T) {
	installRecorder(t)
	ctx, span := Start(context.Background(), "parent")
	defer span.End()
	stripped := WithoutSpan(ctx)
	require.False(t, trace.SpanContextFromContext(stripped).IsValid())
	require.True(t, trace.SpanContextFromContext(ctx).IsValid())
}

func TestBrokenExporterDoesNotPanicOrBlock(t *testing.T) {
	exp := errExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp,
		sdktrace.WithMaxQueueSize(8),
		sdktrace.WithBatchTimeout(time.Millisecond),
		sdktrace.WithExportTimeout(20*time.Millisecond),
	))
	h := &Handle{tp: tp, tracer: tp.Tracer("test"), enabled: true}
	old := Install(h)
	t.Cleanup(func() {
		Install(old)
		_ = tp.Shutdown(context.Background())
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 32; i++ {
			_, span := Start(context.Background(), "noise")
			span.End()
		}
		h.ForceFlush(50 * time.Millisecond)
		h.Shutdown(50 * time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("broken exporter blocked")
	}
}

type errExporter struct{}

func (errExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("export down")
}
func (errExporter) Shutdown(context.Context) error { return errors.New("shutdown down") }

func TestRedactionSyntheticSentinels(t *testing.T) {
	t.Setenv("CURSOR_API_KEY", "cursor-secret-sentinel-value")
	t.Setenv("GITHUB_TOKEN", "ghp_SYNTHETICTESTTOKEN_DO_NOT_USE_123456")
	t.Setenv("GATEWAY_REVIEWER_KEY", "gw-secret-sentinel-value")
	t.Setenv("WEBHOOK_TOKEN", "webhook-secret-sentinel")

	raw := strings.Join([]string{
		"Authorization: Bearer ghp_SYNTHETICTESTTOKEN_DO_NOT_USE_123456",
		"github_pat_SYNTHETIC_TEST_TOKEN_ABCDEF",
		"crsr_SYNTHETIC_TEST_TOKEN_ABCDEF",
		"CURSOR_API_KEY=cursor-secret-sentinel-value",
		"GATEWAY_REVIEWER_KEY=gw-secret-sentinel-value",
		"WEBHOOK_TOKEN=webhook-secret-sentinel",
		`{"diff":"+++ b/secret.go","prompt":"ignore"}`,
	}, "\n")
	got := RedactSecrets(raw)
	require.NotContains(t, got, "ghp_SYNTHETICTESTTOKEN_DO_NOT_USE_123456")
	require.NotContains(t, got, "github_pat_SYNTHETIC_TEST_TOKEN_ABCDEF")
	require.NotContains(t, got, "crsr_SYNTHETIC_TEST_TOKEN_ABCDEF")
	require.NotContains(t, got, "cursor-secret-sentinel-value")
	require.NotContains(t, got, "gw-secret-sentinel-value")
	require.NotContains(t, got, "webhook-secret-sentinel")
	require.Contains(t, got, "[REDACTED]")
}

func TestSanitizeDropsBlockedKeys(t *testing.T) {
	sr := installRecorder(t)
	_, span := Start(context.Background(), "safe",
		attribute.String("http.authorization", "Bearer ghp_SYNTHETICTESTTOKEN_DO_NOT_USE_XX"),
		attribute.String(AttrRepo, "o/r"),
		attribute.String("exception.message", "failed Bearer ghp_SYNTHETICTESTTOKEN_DO_NOT_USE_XX"),
	)
	span.End()
	m := attrMap(spanByName(t, sr, "safe"))
	require.Equal(t, "o/r", m[AttrRepo])
	_, hasAuth := m["http.authorization"]
	require.False(t, hasAuth)
	require.NotContains(t, m["exception.message"], "ghp_")
}

func TestClassifyTaxonomy(t *testing.T) {
	require.Equal(t, ClassUsageLimit, Classify(errors.New("agent failed: usage limit reached for opus"), ""))
	require.Equal(t, ClassTimeoutReviewer, Classify(context.DeadlineExceeded, ""))
	require.Equal(t, ClassDiffFetch, Classify(errors.New("fetching diff from https://api.github.com/repos/o/r/pulls/1: HTTP 404"), ""))
	require.Equal(t, ClassParseEnvelope, Classify(errors.New("CLI did not print a JSON result envelope"), ""))
	require.Equal(t, ClassRateLimit, Classify(errors.New("github list pulls: HTTP 429: rate limit"), ""))
	require.Equal(t, OutcomeUsageLimited, OutcomeFor(errors.New("usage limit"), "", "", 0, false))
	require.Equal(t, OutcomeReviewedClean, OutcomeFor(nil, "", "", 0, true))
	require.Equal(t, OutcomeReviewed, OutcomeFor(nil, "", "", 2, true))
	require.Equal(t, OutcomeSkipped, OutcomeFor(nil, "", "empty diff", 0, true))
}

func TestChildEnvOverridesServiceNameAndInjectsTrace(t *testing.T) {
	installRecorder(t)
	ctx, span := Start(context.Background(), "p")
	defer span.End()
	parent := []string{
		"PATH=/bin",
		"OTEL_SERVICE_NAME=orchestrator",
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318",
		"GITHUB_TOKEN=not-stripped-here",
	}
	got := ChildEnv(parent, ctx, "cursor-reviewer")
	joined := strings.Join(got, "\n")
	require.Contains(t, joined, "OTEL_SERVICE_NAME=cursor-reviewer")
	require.NotContains(t, joined, "OTEL_SERVICE_NAME=orchestrator")
	require.Contains(t, joined, "TRACEPARENT=")
	require.Contains(t, joined, "GITHUB_TOKEN=not-stripped-here")
}

func TestWrapHTTPNoopWhenDisabled(t *testing.T) {
	Install(&Handle{tracer: Current().Tracer()})
	h := WrapHTTP("gw", nil)
	require.Nil(t, h)
}

func TestInitInvalidEndpointFallsBack(t *testing.T) {
	old := Current()
	t.Cleanup(func() {
		Install(old)
	})
	// An unusable endpoint must not prevent the process from running.
	h := Init(context.Background(), Options{ServiceName: "t", Enabled: true, Endpoint: "http://127.0.0.1:1", Protocol: "http/protobuf"})
	// Setup typically succeeds (export fails later); either way it must not panic.
	require.NotNil(t, h)
	_, span := Start(context.Background(), "x")
	span.End()
	h.ForceFlush(50 * time.Millisecond)
	h.Shutdown(50 * time.Millisecond)
}
