package shared

import (
	"context"
	"log"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// InitTracing installs the global tracer provider: OTLP/HTTP (protobuf) to
// OTEL_EXPORTER_OTLP_ENDPOINT (the collector), batching, W3C trace-context + baggage propagation.
// The service name defaults to name and can be overridden with OTEL_SERVICE_NAME. This replaces
// the Node services' `--require @opentelemetry/auto-instrumentations-node/register`; the
// instrumentation itself is explicit (otelhttp, otelpgx, redisotel, and the AMQP spans here).
func InitTracing(ctx context.Context, name string) func() {
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		log.Fatalf("otel exporter: %v", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", name)),
		resource.WithFromEnv(), // OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES win
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		log.Printf("otel resource: %v", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(time.Second)), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx) // flush buffered spans on exit
	}
}
