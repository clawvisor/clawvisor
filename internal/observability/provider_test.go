package observability

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/clawvisor/clawvisor/pkg/config"
)

// TestOTelDisabledByDefault asserts that a fresh Default() config leaves the
// global providers at their no-op defaults, Start returns a usable no-op
// shutdown, and no background goroutines are leaked.
func TestOTelDisabledByDefault(t *testing.T) {
	// Capture globals before Start; they must be unchanged after.
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()

	// A disabled provider must not be an SDK provider.
	if _, ok := beforeTP.(*sdktrace.TracerProvider); ok {
		t.Skip("global tracer provider already set by another test; cannot assert no-op default")
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default().Observability.OTel

	shutdown, err := Start(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("Start(disabled): %v", err)
	}
	if shutdown == nil {
		t.Fatal("Start returned nil shutdown")
	}

	if got := otel.GetTracerProvider(); got != beforeTP {
		t.Error("disabled Start registered a tracer provider")
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Error("disabled Start registered an SDK tracer provider")
	}
	if got := otel.GetMeterProvider(); got != beforeMP {
		t.Error("disabled Start registered a meter provider")
	}
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); ok {
		t.Error("disabled Start registered an SDK meter provider")
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Allow any (unexpected) goroutine to wind down before counting.
	time.Sleep(20 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > before {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}
}

// TestNewInstrumentsWithNoopMeter verifies the instrument set constructs
// cleanly against the global (no-op) meter.
func TestNewInstrumentsWithNoopMeter(t *testing.T) {
	inst, err := NewInstruments(otel.GetMeterProvider().Meter(TracerName))
	if err != nil {
		t.Fatalf("NewInstruments: %v", err)
	}
	if inst == nil || inst.LLMRequests == nil || inst.LLMRequestDur == nil {
		t.Fatal("NewInstruments returned incomplete instrument set")
	}
}
