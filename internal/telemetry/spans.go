package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Current().Tracer().Start(ctx, name, trace.WithAttributes(Sanitize(attrs)...))
}

func StartLinked(ctx context.Context, name string, link trace.Link, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{trace.WithAttributes(Sanitize(attrs)...)}
	if link.SpanContext.IsValid() {
		opts = append(opts, trace.WithLinks(link))
	}
	return Current().Tracer().Start(ctx, name, opts...)
}

// StartRoot starts a new trace (used for cycle markers so review traces are
// not children of the poller). The span is intended to be ended immediately.
func StartRoot(ctx context.Context, name string, links []trace.Link, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithAttributes(Sanitize(attrs)...),
	}
	if len(links) > 0 {
		opts = append(opts, trace.WithLinks(links...))
	}
	return Current().Tracer().Start(ctx, name, opts...)
}

func End(span trace.Span, err error, stderr string) {
	if span == nil {
		return
	}
	if err != nil {
		class := Classify(err, stderr)
		span.SetAttributes(
			attribute.String(AttrErrorClass, class),
			attribute.String(AttrErrorType, class),
		)
		span.RecordError(err, trace.WithAttributes(
			attribute.String("exception.message", SafeMessage(err.Error())),
		))
		span.SetStatus(codes.Error, class)
	}
	span.End()
}

func RecordAttemptStarted(ctx context.Context, kind string, corr Correlation, extra ...attribute.KeyValue) {
	attrs := append(corr.Attrs(), attribute.String(AttrAttemptKind, kind))
	attrs = append(attrs, extra...)
	_, span := Start(ctx, SpanAttemptStarted, attrs...)
	span.End()
	FlushAttempt()
}

func RecordAttemptFinished(ctx context.Context, kind, outcome, errClass string, corr Correlation, extra ...attribute.KeyValue) {
	attrs := append(corr.Attrs(),
		attribute.String(AttrAttemptKind, kind),
		attribute.String(AttrOutcome, outcome),
	)
	if errClass != "" {
		attrs = append(attrs, attribute.String(AttrErrorClass, errClass), attribute.String(AttrErrorType, errClass))
	}
	attrs = append(attrs, extra...)
	_, span := Start(ctx, SpanAttemptFinished, attrs...)
	SetOutcome(span, outcome, errClass, "")
	span.End()
}

func RecordCycleStarted(ctx context.Context, corr Correlation, pollerLink string) (traceparent string) {
	var links []trace.Link
	if l, ok := LinkFromTraceParent(pollerLink); ok {
		links = append(links, l)
	}
	attrs := append(corr.Attrs(),
		attribute.String(AttrGenAIOp, "invoke_workflow"),
	)
	ctx, span := StartRoot(ctx, SpanCycleStarted, links, attrs...)
	traceparent = TraceParent(ctx)
	span.End()
	FlushAttempt()
	return traceparent
}

func RecordCycleFinished(ctx context.Context, corr Correlation, outcome, errClass string, extra ...attribute.KeyValue) {
	attrs := append(corr.Attrs(), extra...)
	_, span := Start(ctx, SpanCycleFinished, attrs...)
	SetOutcome(span, outcome, errClass, "")
	span.End()
	_, out := Start(ctx, SpanReviewOutcome, attrs...)
	SetOutcome(out, outcome, errClass, "")
	out.End()
}

func HeartbeatEvent(span trace.Span, n int, last time.Time) {
	if span == nil || !span.IsRecording() {
		return
	}
	if n%3 != 0 { // 10s ticker → one event per 30s
		return
	}
	span.AddEvent("agentflow.reviewer.heartbeat", trace.WithAttributes(
		attribute.Int("agentflow.heartbeat.n", n),
		attribute.Int64("agentflow.heartbeat.age_s", int64(time.Since(last).Seconds())),
	))
}
