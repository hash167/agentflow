package telemetry

import (
	"log"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"
)

// TemporalInterceptor returns a replay-safe tracing interceptor, or nil when
// tracing is disabled. Register on both the Temporal client and the worker.
func TemporalInterceptor() interceptor.Interceptor {
	h := Current()
	if !h.Enabled() {
		return nil
	}
	i, err := opentelemetry.NewTracingInterceptor(opentelemetry.TracerOptions{
		Tracer:            h.Tracer(),
		DisableBaggage:    true, // we set correlation attrs explicitly; no user data in baggage
		TextMapPropagator: w3c,
	})
	if err != nil {
		log.Printf("[telemetry] temporal interceptor: %v", err)
		return nil
	}
	return i
}

func WithClientInterceptors(existing []interceptor.ClientInterceptor) []interceptor.ClientInterceptor {
	if i := TemporalInterceptor(); i != nil {
		return append(existing, i)
	}
	return existing
}

// WrapHTTP extracts inbound W3C traceparent on /mcp. Health probes are skipped
// so they do not flood Honeycomb. Bodies are not recorded.
func WrapHTTP(operation string, h http.Handler) http.Handler {
	if !Current().Enabled() {
		return h
	}
	return otelhttp.NewHandler(h, operation,
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path == "/mcp"
		}),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
		otelhttp.WithTracerProvider(Current().tp),
		otelhttp.WithPropagators(w3c),
	)
}
