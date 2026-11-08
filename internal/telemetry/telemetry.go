package telemetry

import (
	"context"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ServiceVersion is overwritten at link time (-X) and/or via OTEL_SERVICE_VERSION.
var ServiceVersion = "dev"

const tracerName = "agentflow"

var current atomic.Pointer[Handle]

// Handle is the process tracer. Disabled handles are no-ops.
type Handle struct {
	tp      *sdktrace.TracerProvider
	tracer  trace.Tracer
	enabled bool
}

func init() {
	current.Store(&Handle{tracer: noop.NewTracerProvider().Tracer(tracerName)})
}

func Current() *Handle {
	if h := current.Load(); h != nil {
		return h
	}
	return &Handle{tracer: noop.NewTracerProvider().Tracer(tracerName)}
}

func (h *Handle) Enabled() bool {
	return h != nil && h.enabled
}

func (h *Handle) Tracer() trace.Tracer {
	if h == nil || h.tracer == nil {
		return noop.NewTracerProvider().Tracer(tracerName)
	}
	return h.tracer
}

// Install swaps the process handle. Tests use this with an in-memory recorder.
func Install(h *Handle) *Handle {
	if h == nil {
		h = &Handle{tracer: noop.NewTracerProvider().Tracer(tracerName)}
	}
	old := current.Swap(h)
	if h.tp != nil {
		otel.SetTracerProvider(h.tp)
		otel.SetTextMapPropagator(propagation.TraceContext{})
	}
	return old
}

// Options configure export. The default is a no-op: no endpoint, no export.
type Options struct {
	ServiceName string
	Version     string
	Environment string
	RunKind     string
	Endpoint    string
	Protocol    string // "http/protobuf" (default) or "grpc"
	Enabled     bool
}

func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func disabledByEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AGENTFLOW_TRACING")))
	if v == "0" || v == "false" || v == "off" {
		return true
	}
	return strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true")
}

// Init installs a tracer. Missing/empty endpoint → no-op (backward compatible).
// A broken exporter is logged and replaced with a no-op so reviews still run.
func Init(ctx context.Context, opts Options) *Handle {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Printf("[telemetry] export error: %v", err)
	}))
	otel.SetTextMapPropagator(propagation.TraceContext{})

	if disabledByEnv() {
		h := &Handle{tracer: noop.NewTracerProvider().Tracer(tracerName)}
		Install(h)
		log.Printf("[telemetry] disabled (AGENTFLOW_TRACING/OTEL_SDK_DISABLED)")
		return h
	}

	envEndpoint := envFirst("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	endpoint := envEndpoint
	if endpoint == "" {
		endpoint = strings.TrimSpace(opts.Endpoint)
	}
	// No-op unless ops set an env endpoint or config supplies one.
	if envEndpoint == "" && !opts.Enabled && strings.TrimSpace(opts.Endpoint) == "" {
		h := &Handle{tracer: noop.NewTracerProvider().Tracer(tracerName)}
		Install(h)
		return h
	}
	if endpoint == "" {
		h := &Handle{tracer: noop.NewTracerProvider().Tracer(tracerName)}
		Install(h)
		if opts.Enabled {
			log.Printf("[telemetry] enabled but no OTLP endpoint; staying no-op")
		}
		return h
	}

	service := envFirst("OTEL_SERVICE_NAME")
	if service == "" {
		service = opts.ServiceName
	}
	if service == "" {
		service = "agentflow"
	}
	version := envFirst("OTEL_SERVICE_VERSION", "AGENTFLOW_VERSION")
	if version == "" {
		version = opts.Version
	}
	if version == "" {
		version = ServiceVersion
	}
	envName := envFirst("OTEL_RESOURCE_DEPLOYMENT_ENVIRONMENT", "AGENTFLOW_ENVIRONMENT", "DEPLOYMENT_ENVIRONMENT")
	if envName == "" {
		envName = opts.Environment
	}
	runKind := envFirst("AGENTFLOW_RUN_KIND")
	if runKind == "" {
		runKind = opts.RunKind
	}
	if runKind == "" {
		runKind = RunKindProd
	}
	protocol := envFirst("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL")
	if protocol == "" {
		protocol = opts.Protocol
	}

	exp, err := newExporter(ctx, endpoint, protocol)
	if err != nil {
		log.Printf("[telemetry] otlp exporter setup failed (reviews will continue untraced): %v", err)
		h := &Handle{tracer: noop.NewTracerProvider().Tracer(tracerName)}
		Install(h)
		return h
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			attribute.String("service.name", service),
			attribute.String("service.version", version),
			attribute.String("deployment.environment", envName),
			attribute.String("deployment.environment.name", envName),
			attribute.String(AttrRunKind, runKind),
		),
	)
	if err != nil {
		log.Printf("[telemetry] resource: %v", err)
		res = resource.NewSchemaless(
			attribute.String("service.name", service),
			attribute.String("service.version", version),
			attribute.String("deployment.environment", envName),
			attribute.String(AttrRunKind, runKind),
		)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exp,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithMaxExportBatchSize(256),
			sdktrace.WithBatchTimeout(2*time.Second),
			sdktrace.WithExportTimeout(5*time.Second),
		),
	)
	h := &Handle{tp: tp, tracer: tp.Tracer(tracerName), enabled: true}
	Install(h)
	log.Printf("[telemetry] otlp traces → %s service=%s env=%s version=%s run_kind=%s", endpoint, service, envName, version, runKind)
	return h
}

func newExporter(ctx context.Context, endpoint, protocol string) (sdktrace.SpanExporter, error) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	useGRPC := protocol == "grpc" || (strings.Contains(endpoint, ":4317") && protocol != "http/protobuf" && protocol != "http/json")
	if useGRPC {
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(stripScheme(endpoint))}
		if strings.HasPrefix(endpoint, "http://") || !strings.Contains(endpoint, "https://") {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(ensureURL(endpoint))}
	if strings.HasPrefix(endpoint, "http://") || (!strings.Contains(endpoint, "://") && !strings.Contains(endpoint, "443")) {
		opts = []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(stripScheme(endpoint)),
			otlptracehttp.WithInsecure(),
		}
	}
	return otlptracehttp.New(ctx, opts...)
}

func stripScheme(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	e = strings.TrimPrefix(e, "https://")
	e = strings.TrimPrefix(e, "http://")
	e = strings.TrimSuffix(e, "/v1/traces")
	return e
}

func ensureURL(endpoint string) string {
	if strings.Contains(endpoint, "://") {
		return endpoint
	}
	return "http://" + endpoint
}

func (h *Handle) ForceFlush(timeout time.Duration) {
	if h == nil || h.tp == nil {
		return
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := h.tp.ForceFlush(ctx); err != nil {
		log.Printf("[telemetry] flush: %v", err)
	}
}

func (h *Handle) Shutdown(timeout time.Duration) {
	if h == nil || h.tp == nil {
		return
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := h.tp.Shutdown(ctx); err != nil {
		log.Printf("[telemetry] shutdown: %v", err)
	}
}

// FlushAttempt exports start-marker spans quickly so a later crash still
// leaves evidence of the attempt. Bounded and non-blocking on the hot path
// because ForceFlush uses a short timeout.
func FlushAttempt() {
	Current().ForceFlush(200 * time.Millisecond)
}
