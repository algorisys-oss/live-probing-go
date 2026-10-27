package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"liveprobe/internal/core"
)

// Port of packages/server/src/alert-webhook.test.ts. The transport seam is an http.RoundTripper.

func firing(id string) FiringAlert {
	return FiringAlert{Alert: core.Alert{ID: id, Kind: "service", Target: "order", Metric: "error-rate", Severity: "error",
		Value: 0.2, Threshold: 0.05, Message: "order error rate 20% (20/100)"}, Since: 1000}
}

type call struct {
	url, method string
	body        map[string]any
}

// stubTransport records calls and replays a script of status codes (0 = transport error).
type stubTransport struct {
	mu     sync.Mutex
	script []int
	calls  []call
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	s.calls = append(s.calls, call{r.URL.String(), r.Method, body})
	step := s.script[min(len(s.calls)-1, len(s.script)-1)]
	if step == 0 {
		return nil, errors.New("ECONNREFUSED")
	}
	return &http.Response{StatusCode: step, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func opts(url string, rt http.RoundTripper) WebhookOptions {
	o := DefaultWebhookOptions(url)
	o.Client = &http.Client{Transport: rt}
	o.Sleep = func(time.Duration) {}
	return o
}

func TestWebhookPostsAlertJSON(t *testing.T) {
	st := &stubTransport{script: []int{200}}
	o := opts("http://hook.test/a", st)
	o.Now = func() int64 { return 5000 }
	if !NewWebhookNotifier(o).Notify(firing("service:order:error-rate")) {
		t.Fatal("should deliver")
	}
	if len(st.calls) != 1 || st.calls[0].url != "http://hook.test/a" || st.calls[0].method != "POST" {
		t.Fatalf("calls: %+v", st.calls)
	}
	b := st.calls[0].body
	alert := b["alert"].(map[string]any)
	if b["event"] != "alert.firing" || b["firedAt"] != float64(5000) || alert["id"] != "service:order:error-rate" || alert["severity"] != "error" || alert["since"] != float64(1000) {
		t.Errorf("body: %+v", b)
	}
}

func TestWebhookRetriesWithBackoff(t *testing.T) {
	st := &stubTransport{script: []int{0, 0, 200}}
	var sleeps []time.Duration
	o := opts("http://hook.test", st)
	o.Retries, o.Backoff = 2, 100*time.Millisecond
	o.Sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	if !NewWebhookNotifier(o).Notify(firing("a")) {
		t.Fatal("should eventually deliver")
	}
	if len(st.calls) != 3 || !slices.Equal(sleeps, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}) {
		t.Errorf("calls %d sleeps %v", len(st.calls), sleeps)
	}
}

func TestWebhookGivesUpAndReports(t *testing.T) {
	st := &stubTransport{script: []int{0}}
	var errs []error
	o := opts("http://hook.test", st)
	o.OnError = func(err error, _ FiringAlert) { errs = append(errs, err) }
	if NewWebhookNotifier(o).Notify(firing("a")) {
		t.Fatal("should fail")
	}
	if len(st.calls) != 3 || len(errs) != 1 {
		t.Errorf("calls %d errors %d", len(st.calls), len(errs))
	}
}

func TestWebhookNon2xxRetried(t *testing.T) {
	st := &stubTransport{script: []int{503, 500, 500}}
	if NewWebhookNotifier(opts("http://hook.test", st)).Notify(firing("a")) {
		t.Fatal("should fail")
	}
	if len(st.calls) != 3 {
		t.Errorf("calls %d", len(st.calls))
	}
}

func TestWebhookCooldown(t *testing.T) {
	st := &stubTransport{script: []int{200}}
	var clock int64
	o := opts("http://hook.test", st)
	o.Cooldown = 60 * time.Second
	o.Now = func() int64 { return clock }
	n := NewWebhookNotifier(o)
	clock = 1000
	if !n.Notify(firing("x")) {
		t.Fatal("first fire")
	}
	clock = 30_000 // within cooldown
	if n.Notify(firing("x")) || len(st.calls) != 1 {
		t.Fatal("suppressed within cooldown")
	}
	clock = 70_000
	if !n.Notify(firing("x")) || len(st.calls) != 2 {
		t.Fatal("allowed after cooldown")
	}
}

func TestWebhookCooldownPerID(t *testing.T) {
	st := &stubTransport{script: []int{200}}
	o := opts("http://hook.test", st)
	o.Now = func() int64 { return 1000 }
	n := NewWebhookNotifier(o)
	if !n.Notify(firing("service:order:error-rate")) || !n.Notify(firing("edge:gw->order:latency")) || len(st.calls) != 2 {
		t.Fatal("distinct ids fire independently")
	}
}

type hangTransport struct{ sawContext bool }

func (h *hangTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	_, h.sawContext = r.Context().Deadline()
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestWebhookTimeoutAbortsHungRequest(t *testing.T) {
	h := &hangTransport{}
	o := opts("http://hook.test", h)
	o.Timeout, o.Retries = 20*time.Millisecond, 0
	start := time.Now()
	if NewWebhookNotifier(o).Notify(firing("a")) {
		t.Fatal("hung request must fail")
	}
	if !h.sawContext || time.Since(start) > 2*time.Second {
		t.Errorf("deadline=%v elapsed=%v", h.sawContext, time.Since(start))
	}
}
