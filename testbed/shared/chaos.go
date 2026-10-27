// Package shared is the Shopwave testbed's common library: env, fault injection, client
// resilience, message contracts, and the OpenTelemetry-instrumented HTTP / Postgres / Redis /
// RabbitMQ plumbing every service uses. Port of ../live-probing/testbed/packages/shared.
package shared

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"time"
)

// Env returns $name, else fallback.
func Env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// EnvInt returns $name as an integer, else fallback. A malformed value is fatal at startup.
func EnvInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		panic(fmt.Sprintf("environment variable %s must be an integer, got %q", name, raw))
	}
	return n
}

// --- fault injection -----------------------------------------------------------------------

// FaultConfig deliberately makes a service slow or failing so LiveProbe's health colouring,
// alerts and dependency matrix have real red/amber to show. Every knob defaults to zero.
// Testbed-only; never ship this in a real service.
type FaultConfig struct {
	LatencyMs       int     // added latency when a latency fault hits
	LatencyJitterMs int     // uniform extra 0..jitter on top
	LatencyRate     float64 // 0..1 probability of the latency hit
	FailRate        float64 // 0..1 probability the request fails outright
	FailStatus      int     // HTTP status an injected failure maps to (default 503, retryable)
}

var NoFault = FaultConfig{FailStatus: 503}

// InjectedFault is a failure injected on purpose; the HTTP layer maps it to StatusCode.
type InjectedFault struct{ StatusCode int }

func (e *InjectedFault) Error() string { return fmt.Sprintf("injected fault (%d)", e.StatusCode) }

func rate(name string) float64 {
	n, err := strconv.ParseFloat(os.Getenv(name), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0
	}
	return math.Min(1, math.Max(0, n))
}

// FaultFromEnv reads ${prefix}_LATENCY_MS / _LATENCY_JITTER_MS / _LATENCY_RATE / _FAIL_RATE /
// _FAIL_STATUS. Anything unset is zero, so the config is inert unless configured.
func FaultFromEnv(prefix string) FaultConfig {
	return FaultConfig{
		LatencyMs:       EnvInt(prefix+"_LATENCY_MS", 0),
		LatencyJitterMs: EnvInt(prefix+"_LATENCY_JITTER_MS", 0),
		LatencyRate:     rate(prefix + "_LATENCY_RATE"),
		FailRate:        rate(prefix + "_FAIL_RATE"),
		FailStatus:      EnvInt(prefix+"_FAIL_STATUS", 503),
	}
}

// Active reports whether the config would ever do anything.
func (c FaultConfig) Active() bool { return (c.LatencyRate > 0 && c.LatencyMs > 0) || c.FailRate > 0 }

// FaultDeps are seams for tests.
type FaultDeps struct {
	Random func() float64
	Sleep  func(context.Context, time.Duration)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// ApplyFault maybe adds latency, then maybe returns an *InjectedFault. Latency first, so a slow
// and failed request spends the time before failing, like a real overloaded dependency.
func ApplyFault(ctx context.Context, c FaultConfig, d FaultDeps) error {
	random, sleep := d.Random, d.Sleep
	if random == nil {
		random = rand.Float64
	}
	if sleep == nil {
		sleep = sleepCtx
	}
	if c.LatencyMs > 0 && c.LatencyRate > 0 && random() < c.LatencyRate {
		jitter := 0.0
		if c.LatencyJitterMs > 0 {
			jitter = random() * float64(c.LatencyJitterMs)
		}
		sleep(ctx, time.Duration((float64(c.LatencyMs)+jitter)*float64(time.Millisecond)))
	}
	if c.FailRate > 0 && random() < c.FailRate {
		return &InjectedFault{StatusCode: c.FailStatus}
	}
	return nil
}
