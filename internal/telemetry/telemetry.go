// Package telemetry configures OpenTelemetry tracing for the gateway.
package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Setup creates spans without exporting them, so trace IDs exist and propagate with no collector.
func Setup() (shutdown func()) {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return func() { _ = tp.Shutdown(context.Background()) }
}
