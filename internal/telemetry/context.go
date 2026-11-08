package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	envTraceParent = "TRACEPARENT"
	envTraceState  = "TRACESTATE"
)

var w3c = propagation.TraceContext{}

type envCarrier map[string]string

func (c envCarrier) Get(key string) string { return c[strings.ToLower(key)] }
func (c envCarrier) Set(key, value string) { c[strings.ToLower(key)] = value }
func (c envCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// InjectEnv returns TRACEPARENT/TRACESTATE entries for a subprocess.
func InjectEnv(ctx context.Context) []string {
	car := envCarrier{}
	w3c.Inject(ctx, car)
	var out []string
	if v := car.Get("traceparent"); v != "" {
		out = append(out, envTraceParent+"="+v)
	}
	if v := car.Get("tracestate"); v != "" {
		out = append(out, envTraceState+"="+v)
	}
	return out
}

// ExtractEnv continues a trace from TRACEPARENT in the process environment.
func ExtractEnv(ctx context.Context, environ []string) context.Context {
	car := envCarrier{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(k) {
		case envTraceParent:
			car.Set("traceparent", v)
		case envTraceState:
			car.Set("tracestate", v)
		}
	}
	if car.Get("traceparent") == "" {
		return ctx
	}
	return w3c.Extract(ctx, car)
}

// TraceParent returns the W3C traceparent for ctx, or "".
func TraceParent(ctx context.Context) string {
	car := envCarrier{}
	w3c.Inject(ctx, car)
	return car.Get("traceparent")
}

// ContextFromTraceParent returns ctx with the remote span context extracted
// from a W3C traceparent. The extracted context is for linking, not parenting.
func ContextFromTraceParent(ctx context.Context, traceparent string) context.Context {
	traceparent = strings.TrimSpace(traceparent)
	if traceparent == "" {
		return ctx
	}
	car := envCarrier{}
	car.Set("traceparent", traceparent)
	return w3c.Extract(ctx, car)
}

// LinkFromTraceParent builds an OTel link to the poller (or previous cycle)
// span encoded in traceparent. Returns a zero Link if the value is empty/invalid.
func LinkFromTraceParent(traceparent string) (trace.Link, bool) {
	ctx := ContextFromTraceParent(context.Background(), traceparent)
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: sc, Attributes: nil}, true
}

// WithoutSpan returns a context that keeps deadlines/values but has no span,
// so SignalWithStart does not parent a review workflow under the poller trace.
func WithoutSpan(ctx context.Context) context.Context {
	return trace.ContextWithSpanContext(ctx, trace.SpanContext{})
}

// ChildEnv copies parent, overrides OTEL_SERVICE_NAME, and injects TRACEPARENT.
// It does not strip unrelated secrets (that is a separate env-allowlist change).
func ChildEnv(parent []string, ctx context.Context, serviceName string) []string {
	out := make([]string, 0, len(parent)+4)
	for _, kv := range parent {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		switch strings.ToUpper(k) {
		case "OTEL_SERVICE_NAME", envTraceParent, envTraceState:
			continue
		}
		out = append(out, kv)
	}
	if serviceName != "" {
		out = append(out, "OTEL_SERVICE_NAME="+serviceName)
	}
	out = append(out, InjectEnv(ctx)...)
	return out
}
