package telemetry

import (
	"context"
	"errors"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// SetupTest installs a synchronous in-memory exporter. For tests only.
func SetupTest() (sr *tracetest.SpanRecorder, cleanup func()) {
	sr = tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	h := &Handle{tp: tp, tracer: tp.Tracer("test"), enabled: true}
	old := Install(h)
	return sr, func() {
		Install(old)
		_ = tp.Shutdown(context.Background())
	}
}

func EndedNames(sr *tracetest.SpanRecorder) []string {
	var out []string
	for _, s := range sr.Ended() {
		out = append(out, s.Name())
	}
	return out
}

func AttrsOf(sr *tracetest.SpanRecorder, name string) map[string]string {
	for _, s := range sr.Ended() {
		if s.Name() != name {
			continue
		}
		m := map[string]string{}
		for _, a := range s.Attributes() {
			m[string(a.Key)] = a.Value.Emit()
		}
		return m
	}
	return nil
}

// SetupBroken installs a tracer whose exporter always fails. Reviews must still succeed.
func SetupBroken() (cleanup func()) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(failingExporter{},
		sdktrace.WithMaxQueueSize(8),
		sdktrace.WithBatchTimeout(time.Millisecond),
		sdktrace.WithExportTimeout(20*time.Millisecond),
	))
	h := &Handle{tp: tp, tracer: tp.Tracer("test"), enabled: true}
	old := Install(h)
	return func() {
		Install(old)
		_ = tp.Shutdown(context.Background())
	}
}

type failingExporter struct{}

func (failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("export down")
}
func (failingExporter) Shutdown(context.Context) error { return errors.New("shutdown down") }

func DumpAttrs(sr *tracetest.SpanRecorder) string {
	var b []byte
	for _, s := range sr.Ended() {
		b = append(b, s.Name()...)
		b = append(b, ':')
		for _, a := range s.Attributes() {
			b = append(b, ' ')
			b = append(b, string(a.Key)...)
			b = append(b, '=')
			b = append(b, a.Value.Emit()...)
		}
		for _, ev := range s.Events() {
			b = append(b, " event:"...)
			b = append(b, ev.Name...)
			for _, a := range ev.Attributes {
				b = append(b, ' ')
				b = append(b, string(a.Key)...)
				b = append(b, '=')
				b = append(b, a.Value.Emit()...)
			}
		}
		b = append(b, '\n')
	}
	return string(b)
}
