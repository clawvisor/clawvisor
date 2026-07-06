// Package observability provides an OpenTelemetry OTLP export layer for
// operational traces and metrics from the Clawvisor server (the proxy-lite
// pipeline, the skill gateway, and the runtime proxy).
//
// This is distinct from internal/telemetry, which sends anonymous product
// analytics to telemetry.clawvisor.com. Observability exports operator-facing
// traces/metrics to an OTLP endpoint the customer already runs.
//
// When disabled (the default), Start does nothing and instrumented code paths
// go through the global no-op providers, so instrumentation has zero cost.
package observability

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/version"
)

// Start initializes the global OTel trace and meter providers from cfg and
// registers them via otel.SetTracerProvider / otel.SetMeterProvider. It
// returns a shutdown function that flushes and stops the providers.
//
// When cfg.Enabled is false it returns a no-op shutdown and does nothing —
// the global providers stay at their no-op defaults.
func Start(ctx context.Context, cfg config.OTelConfig, logger *slog.Logger) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if !cfg.Enabled {
		return noop, nil
	}

	// Merge onto Default() with a schemaless resource so we don't collide
	// with Default()'s (SDK-versioned) schema URL — resource.Merge rejects
	// two different non-empty schema URLs.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(serviceName(cfg)),
			semconv.ServiceVersion(version.Version),
		),
	)
	if err != nil {
		return noop, fmt.Errorf("observability: build resource: %w", err)
	}

	traceExp, err := newTraceExporter(ctx, cfg)
	if err != nil {
		return noop, fmt.Errorf("observability: trace exporter: %w", err)
	}
	metricExp, err := newMetricExporter(ctx, cfg)
	if err != nil {
		_ = traceExp.Shutdown(ctx)
		return noop, fmt.Errorf("observability: metric exporter: %w", err)
	}

	ratio := cfg.TraceSampleRatio
	if ratio <= 0 {
		ratio = 0
	} else if ratio > 1 {
		ratio = 1
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)

	interval := time.Duration(cfg.MetricsIntervalSec) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	mp := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(metricExp, metric.WithInterval(interval))),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	logger.Info("observability: OTel export enabled",
		"endpoint", cfg.Endpoint,
		"protocol", protocol(cfg),
		"service", serviceName(cfg),
	)

	return func(ctx context.Context) error {
		var firstErr error
		if e := tp.Shutdown(ctx); e != nil && firstErr == nil {
			firstErr = e
		}
		if e := mp.Shutdown(ctx); e != nil && firstErr == nil {
			firstErr = e
		}
		return firstErr
	}, nil
}

func newTraceExporter(ctx context.Context, cfg config.OTelConfig) (*otlptrace.Exporter, error) {
	if protocol(cfg) == "http" {
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		return otlptracehttp.New(ctx, opts...)
	}
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}
	return otlptracegrpc.New(ctx, opts...)
}

func newMetricExporter(ctx context.Context, cfg config.OTelConfig) (metric.Exporter, error) {
	if protocol(cfg) == "http" {
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Headers))
		}
		return otlpmetrichttp.New(ctx, opts...)
	}
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.Headers))
	}
	return otlpmetricgrpc.New(ctx, opts...)
}

func protocol(cfg config.OTelConfig) string {
	if cfg.Protocol == "http" {
		return "http"
	}
	return "grpc"
}

func serviceName(cfg config.OTelConfig) string {
	if cfg.ServiceName != "" {
		return cfg.ServiceName
	}
	return "clawvisor"
}
