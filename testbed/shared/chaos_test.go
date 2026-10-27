package shared

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Port of packages/shared/src/chaos.test.ts.

func deps(random float64, slept *time.Duration) FaultDeps {
	return FaultDeps{
		Random: func() float64 { return random },
		Sleep: func(_ context.Context, d time.Duration) {
			if slept != nil {
				*slept = d
			}
		},
	}
}

func TestInertFaultDoesNothing(t *testing.T) {
	var slept time.Duration
	if err := ApplyFault(context.Background(), NoFault, deps(0, &slept)); err != nil || slept != 0 {
		t.Errorf("err=%v slept=%v", err, slept)
	}
}

func TestFailRateOneThrowsInjectedFault(t *testing.T) {
	cfg := NoFault
	cfg.FailRate, cfg.FailStatus = 1, 503
	err := ApplyFault(context.Background(), cfg, deps(0.5, nil))
	var f *InjectedFault
	if !errors.As(err, &f) || f.StatusCode != 503 {
		t.Errorf("err = %v", err)
	}
}

func TestFailRateGatesOnTheDraw(t *testing.T) {
	cfg := NoFault
	cfg.FailRate = 0.5
	if err := ApplyFault(context.Background(), cfg, deps(0.9, nil)); err != nil {
		t.Errorf("0.9 >= 0.5 should pass: %v", err)
	}
	if err := ApplyFault(context.Background(), cfg, deps(0.1, nil)); err == nil {
		t.Error("0.1 < 0.5 should fail")
	}
}

func TestLatencySleepsLatencyPlusJitter(t *testing.T) {
	cfg := NoFault
	cfg.LatencyMs, cfg.LatencyJitterMs, cfg.LatencyRate = 800, 400, 1
	var slept time.Duration
	if err := ApplyFault(context.Background(), cfg, deps(0.5, &slept)); err != nil {
		t.Fatal(err)
	}
	if slept != 1000*time.Millisecond {
		t.Errorf("slept %v, want 1s", slept)
	}
}

func TestLatencyThenFailure(t *testing.T) {
	cfg := NoFault
	cfg.LatencyMs, cfg.LatencyRate, cfg.FailRate = 100, 1, 1
	var slept time.Duration
	if err := ApplyFault(context.Background(), cfg, deps(0, &slept)); err == nil {
		t.Error("should fail")
	}
	if slept != 100*time.Millisecond {
		t.Errorf("slept %v", slept)
	}
}

func TestFaultFromEnv(t *testing.T) {
	t.Setenv("TEST_FAULT_A_LATENCY_MS", "300")
	t.Setenv("TEST_FAULT_A_FAIL_RATE", "2") // clamps to 1
	t.Setenv("TEST_FAULT_A_FAIL_STATUS", "500")
	cfg := FaultFromEnv("TEST_FAULT_A")
	if cfg.LatencyMs != 300 || cfg.FailRate != 1 || cfg.FailStatus != 500 || !cfg.Active() {
		t.Errorf("cfg %+v", cfg)
	}
	if FaultFromEnv("TEST_FAULT_UNSET_ZZZ").Active() {
		t.Error("unset prefix should be inert")
	}
}
