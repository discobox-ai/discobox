package server

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// TelemetryOptions controls optional OpenTelemetry server instrumentation.
type TelemetryOptions struct {
	MetricsEnabled       bool
	MetricExportInterval time.Duration
}

func initTelemetry(ctx context.Context, opts TelemetryOptions) (func(context.Context) error, error) {
	if !opts.MetricsEnabled {
		return func(context.Context) error { return nil }, nil
	}
	if opts.MetricExportInterval <= 0 {
		opts.MetricExportInterval = time.Second
	}

	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("initialize OTLP metric exporter: %w", err)
	}

	res := resource.NewWithAttributes(
		"",
		attribute.String("service.name", "discobox-server"),
		attribute.String("service.version", Version),
	)
	provider := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter, metric.WithInterval(opts.MetricExportInterval))),
	)
	otel.SetMeterProvider(provider)

	return provider.Shutdown, nil
}
