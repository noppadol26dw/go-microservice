// OpenTelemetry wiring for the service: tracer/meter providers, OTLP exporters
// pointed at the ADOT collector sidecar, X-Ray-compatible trace IDs and
// propagation, an ECS resource detector, the metric instruments, and helpers to
// carry trace context across the SQS hop. Kept separate from main.go because it
// is a distinct concern with its own dependency surface.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"go.opentelemetry.io/contrib/detectors/aws/ecs"
	"go.opentelemetry.io/contrib/propagators/aws/xray"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationScope names the tracer/meter so telemetry is attributable to
// this service.
const instrumentationScope = "go-microservice"

// setupLogging installs a JSON slog handler as the default logger, wrapped so
// that records carrying a recording span also get trace_id/span_id. Call once,
// before anything logs.
func setupLogging() {
	base := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(&traceHandler{base}))
}

// traceHandler decorates a slog.Handler, adding the current trace_id/span_id
// (when a valid span is in the record's context) so logs correlate with traces.
// Use the *Context log variants (slog.InfoContext, slog.ErrorContext, ...) to
// feed it the span-carrying context.
type traceHandler struct{ slog.Handler }

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		// X-Ray-formatted trace id (1-{8 hex}-{24 hex}) so log lines join to the
		// trace in X-Ray / CloudWatch rather than the raw 32-hex OTel form.
		r.AddAttrs(
			slog.String("trace_id", xrayTraceID(sc.TraceID())),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{h.Handler.WithGroup(name)}
}

// xrayTraceID renders an OpenTelemetry trace ID in AWS X-Ray's textual form:
// "1-" + first 8 hex digits + "-" + the remaining 24. The X-Ray ID generator
// puts the request epoch in those first 8 digits, so this is the value X-Ray and
// CloudWatch use to key a trace.
func xrayTraceID(t trace.TraceID) string {
	s := t.String() // 32 lowercase hex chars
	return "1-" + s[0:8] + "-" + s[8:32]
}

// tracer is the package-wide tracer. It delegates to the global provider, so it
// picks up the real provider installed by setupOTel (and is a safe no-op until
// then).
var tracer = otel.Tracer(instrumentationScope)

// Metric instruments, initialised by initInstruments. They bind to whatever
// MeterProvider is global at init time (the real one after setupOTel, otherwise
// a no-op), so call sites never need nil checks.
var (
	jobsCreated           metric.Int64Counter
	jobProcessingDuration metric.Float64Histogram
)

// setupOTel installs global trace and metric providers that export via OTLP/gRPC
// to the ADOT collector sidecar (endpoint taken from OTEL_EXPORTER_OTLP_ENDPOINT,
// defaulting to localhost:4317). Traces use X-Ray-compatible IDs and the X-Ray
// propagator so they show up correctly in X-Ray and propagate across SQS.
//
// It returns a shutdown function that flushes and stops both providers. Exporter
// creation does not dial eagerly, so this succeeds even when the collector is not
// yet reachable.
func setupOTel(ctx context.Context) (func(context.Context) error, error) {
	// resource.New returns a usable resource even when a detector fails (e.g. the
	// ECS detector when running off-ECS), so detector errors here are non-fatal.
	res, err := resource.New(ctx,
		resource.WithDetectors(ecs.NewResourceDetector()),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(semconv.ServiceName(instrumentationScope)),
	)
	if res == nil {
		res = resource.Default()
	}
	_ = err // partial resource detection is expected off-ECS

	traceExp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
		sdktrace.WithIDGenerator(xray.NewIDGenerator()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(xray.Propagator{})

	metricExp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
	)
	otel.SetMeterProvider(mp)

	shutdown := func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return shutdown, nil
}

// initInstruments creates the metric instruments from the global meter. Safe to
// call even if setupOTel failed — it then binds to no-op instruments.
func initInstruments() error {
	m := otel.Meter(instrumentationScope)
	var err error
	if jobsCreated, err = m.Int64Counter(
		"jobs.created",
		metric.WithDescription("Number of jobs accepted via POST /jobs"),
		metric.WithUnit("{job}"),
	); err != nil {
		return err
	}
	if jobProcessingDuration, err = m.Float64Histogram(
		"job.processing.duration",
		metric.WithDescription("Time to process a job in the worker"),
		metric.WithUnit("s"),
	); err != nil {
		return err
	}
	return nil
}

// otelSQSAttributes serialises the current trace context into SQS message
// attributes so the worker can continue the same trace after the queue hop.
// Returns nil when there is no active context to propagate.
func otelSQSAttributes(ctx context.Context) map[string]sqstypes.MessageAttributeValue {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	attrs := make(map[string]sqstypes.MessageAttributeValue, len(carrier))
	for k, v := range carrier {
		attrs[k] = sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(v),
		}
	}
	return attrs
}

// otelSQSContext rebuilds the trace context that otelSQSAttributes injected,
// from an SQS message's attributes, so processing continues the original trace.
func otelSQSContext(ctx context.Context, attrs map[string]sqstypes.MessageAttributeValue) context.Context {
	carrier := propagation.MapCarrier{}
	for k, v := range attrs {
		if v.StringValue != nil {
			carrier[k] = *v.StringValue
		}
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}
