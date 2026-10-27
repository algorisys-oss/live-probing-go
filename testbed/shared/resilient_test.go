package shared

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Port of packages/shared/src/resilient.test.ts. The transport seam is an http.RoundTripper.

// step is one scripted outcome: a status code, a transport error, or "hang" until cancelled.
type step struct {
	status int
	err    error
	hang   bool
}

type script struct {
	mu    sync.Mutex
	steps []step
	calls int
}

func (s *script) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	st := s.steps[min(s.calls, len(s.steps)-1)]
	s.calls++
	s.mu.Unlock()
	if st.hang {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	if st.err != nil {
		return nil, st.err
	}
	return &http.Response{StatusCode: st.status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func (s *script) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type clock struct{ t time.Time }

func client(s *script, c *clock) *ResilientClient {
	if c == nil {
		c = &clock{t: time.Unix(1000, 0)}
	}
	return &ResilientClient{
		HTTP:     &http.Client{Transport: s},
		Now:      func() time.Time { return c.t },
		Sleep:    func(context.Context, time.Duration) {},
		breakers: map[string]*breaker{},
	}
}

func get(t *testing.T, c *ResilientClient, url string, p Policy) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	res, err := c.Do(req, p)
	if res != nil {
		res.Body.Close()
	}
	return res, err
}

func TestDefaultPolicyIsOnePlainRequest(t *testing.T) {
	s := &script{steps: []step{{status: 200}}}
	res, err := get(t, client(s, nil), "http://svc/a", NoResilience)
	if err != nil || res.StatusCode != 200 || s.count() != 1 {
		t.Errorf("res=%v err=%v calls=%d", res, err, s.count())
	}
}

func TestRetries5xxThenSucceeds(t *testing.T) {
	s := &script{steps: []step{{status: 503}, {status: 500}, {status: 200}}}
	p := NoResilience
	p.Retries = 2
	res, _ := get(t, client(s, nil), "http://svc/a", p)
	if res.StatusCode != 200 || s.count() != 3 {
		t.Errorf("status=%d calls=%d", res.StatusCode, s.count())
	}
}

func TestRetriesTransportErrorThenSucceeds(t *testing.T) {
	s := &script{steps: []step{{err: errors.New("ECONNREFUSED")}, {status: 200}}}
	p := NoResilience
	p.Retries = 1
	res, _ := get(t, client(s, nil), "http://svc/a", p)
	if res.StatusCode != 200 || s.count() != 2 {
		t.Errorf("status=%d calls=%d", res.StatusCode, s.count())
	}
}

func TestDoesNotRetry4xx(t *testing.T) {
	s := &script{steps: []step{{status: 404}, {status: 200}}}
	p := NoResilience
	p.Retries = 3
	res, _ := get(t, client(s, nil), "http://svc/a", p)
	if res.StatusCode != 404 || s.count() != 1 {
		t.Errorf("status=%d calls=%d", res.StatusCode, s.count())
	}
}

func TestRetriesExhaustedReturnsLast5xx(t *testing.T) {
	s := &script{steps: []step{{status: 503}}}
	p := NoResilience
	p.Retries = 2
	res, err := get(t, client(s, nil), "http://svc/a", p)
	if err != nil || res.StatusCode != 503 || s.count() != 3 {
		t.Errorf("res=%v err=%v calls=%d", res, err, s.count())
	}
}

func TestTimeoutAbortsHungRequestAndRetries(t *testing.T) {
	s := &script{steps: []step{{hang: true}, {status: 200}}}
	p := NoResilience
	p.TimeoutMs, p.Retries = 20, 1
	res, _ := get(t, client(s, nil), "http://svc/a", p)
	if res.StatusCode != 200 || s.count() != 2 {
		t.Errorf("status=%d calls=%d", res.StatusCode, s.count())
	}
}

func TestBreakerOpensAndSheds(t *testing.T) {
	s := &script{steps: []step{{status: 503}}}
	c := client(s, nil)
	p := NoResilience
	p.BreakerThreshold, p.BreakerResetMs = 3, 10_000
	for range 3 {
		if res, _ := get(t, c, "http://svc/a", p); res.StatusCode != 503 {
			t.Fatalf("status %d", res.StatusCode)
		}
	}
	_, err := get(t, c, "http://svc/a", p)
	var open *CircuitOpenError
	if !errors.As(err, &open) || s.count() != 3 {
		t.Errorf("err=%v calls=%d (no fetch while open)", err, s.count())
	}
}

func TestBreakerHalfOpensAndCloses(t *testing.T) {
	s := &script{steps: []step{{status: 503}, {status: 503}, {status: 503}, {status: 200}}}
	clk := &clock{t: time.Unix(1000, 0)}
	c := client(s, clk)
	p := NoResilience
	p.BreakerThreshold, p.BreakerResetMs = 3, 5000
	for range 3 {
		get(t, c, "http://svc/a", p)
	}
	if _, err := get(t, c, "http://svc/a", p); err == nil {
		t.Fatal("breaker should be open")
	}
	clk.t = clk.t.Add(6 * time.Second) // past the reset window → half-open trial
	if res, _ := get(t, c, "http://svc/a", p); res.StatusCode != 200 || s.count() != 4 {
		t.Fatalf("trial: status=%d calls=%d", res.StatusCode, s.count())
	}
	clk.t = clk.t.Add(100 * time.Millisecond)
	if res, _ := get(t, c, "http://svc/a", p); res.StatusCode != 200 {
		t.Errorf("closed breaker should pass: %d", res.StatusCode)
	}
}

func TestBreakerIsPerHost(t *testing.T) {
	s := &script{steps: []step{{status: 503}}}
	c := client(s, nil)
	p := NoResilience
	p.BreakerThreshold, p.BreakerResetMs = 1, 10_000
	get(t, c, "http://a-host/x", p)
	if _, err := get(t, c, "http://a-host/x", p); err == nil {
		t.Error("a-host should be open")
	}
	if res, err := get(t, c, "http://b-host/y", p); err != nil || res.StatusCode != 503 {
		t.Errorf("b-host should still call through: %v", err)
	}
}

func TestPolicyFromEnv(t *testing.T) {
	t.Setenv("TEST_RES_A_RETRIES", "2")
	t.Setenv("TEST_RES_A_TIMEOUT_MS", "1500")
	t.Setenv("TEST_RES_A_BREAKER_THRESHOLD", "8")
	p := PolicyFromEnv("TEST_RES_A")
	if p.Retries != 2 || p.TimeoutMs != 1500 || p.BreakerThreshold != 8 {
		t.Errorf("policy %+v", p)
	}
	off := NoResilience
	off.BackoffMs, off.BreakerResetMs = 100, 10_000
	if got := PolicyFromEnv("TEST_RES_UNSET_ZZZ"); got != off {
		t.Errorf("unset policy %+v, want %+v", got, off)
	}
}
