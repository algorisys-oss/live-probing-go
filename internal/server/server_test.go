package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"liveprobe/internal/core"
	"liveprobe/internal/history"
)

// Port of packages/server/src/server.test.ts.

const otlpFixture = `{"resourceSpans":[
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"gateway"}}]},"scopeSpans":[{"spans":[
  {"traceId":"trace-1","spanId":"g1","name":"POST /checkout","kind":2,"startTimeUnixNano":"1000000000","endTimeUnixNano":"1050000000","status":{"code":0}},
  {"traceId":"trace-1","spanId":"g2","parentSpanId":"g1","name":"GET","kind":3,"startTimeUnixNano":"1005000000","endTimeUnixNano":"1045000000","attributes":[{"key":"server.address","value":{"stringValue":"catalog"}}],"status":{"code":0}}]}]},
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"catalog"}}]},"scopeSpans":[{"spans":[
  {"traceId":"trace-1","spanId":"c1","parentSpanId":"g2","name":"GET /products","kind":2,"startTimeUnixNano":"1010000000","endTimeUnixNano":"1040000000","status":{"code":0}},
  {"traceId":"trace-1","spanId":"c2","parentSpanId":"c1","name":"pg.query:SELECT","kind":3,"startTimeUnixNano":"1015000000","endTimeUnixNano":"1030000000","attributes":[{"key":"db.system","value":{"stringValue":"postgresql"}}],"status":{"code":0}},
  {"traceId":"trace-1","spanId":"c3","parentSpanId":"c1","name":"get","kind":3,"startTimeUnixNano":"1031000000","endTimeUnixNano":"1035000000","attributes":[{"key":"db.system","value":{"stringValue":"redis"}}],"status":{"code":0}}]}]}]}`

func start(t *testing.T, o Options) (*Server, string) {
	t.Helper()
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		s.Close()
	})
	return s, ts.URL
}

func post(t *testing.T, url, body string, headers ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

func readWS(t *testing.T, ctx context.Context, c *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestIngestOTLPAndServeRESTAndWebSocket(t *testing.T) {
	_, base := start(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if snap := readWS(t, ctx, c); snap["type"] != "snapshot" {
		t.Fatalf("first message: %v", snap["type"])
	}

	if res := post(t, base+"/v1/traces", otlpFixture); res.StatusCode != 200 {
		t.Fatalf("ingest status %d", res.StatusCode)
	}
	var delta map[string]any
	for delta == nil {
		if m := readWS(t, ctx, c); m["type"] == "traces" {
			delta = m
		}
	}
	tr := delta["traces"].([]any)[0].(map[string]any)
	if tr["traceId"] != "trace-1" || tr["spanCount"] != float64(5) || len(tr["services"].([]any)) != 2 {
		t.Errorf("delta: %+v", tr)
	}

	if _, list := getJSON(t, base+"/api/traces"); len(list["traces"].([]any)) != 1 {
		t.Errorf("list: %+v", list)
	}
	_, det := getJSON(t, base+"/api/traces/trace-1")
	if !regexp.MustCompile(`^sequenceDiagram`).MatchString(det["mermaidSequence"].(string)) ||
		len(det["sequence"].(map[string]any)["messages"].([]any)) < 3 {
		t.Errorf("detail: %+v", det)
	}
	_, topo := getJSON(t, base+"/api/topology")
	if len(topo["topology"].(map[string]any)["edges"].([]any)) < 3 || !strings.HasPrefix(topo["mermaidFlow"].(string), "graph LR") {
		t.Errorf("topology: %+v", topo)
	}
	if status, _ := getJSON(t, base+"/api/traces/nope"); status != 404 {
		t.Errorf("missing trace status %d", status)
	}
}

func nowMicros() int64 { return time.Now().UnixMilli() * 1000 }

func TestHistoryFlushedOnAPIRead(t *testing.T) {
	// A flush interval far longer than the test: only flush-on-read can make the data visible.
	_, base := start(t, Options{HistoryFlush: time.Minute})
	body := `{"events":[{"traceId":"d-1","spanId":"a","participant":"gw","operation":"GET /debounced","kind":"server","startTime":` +
		jsonNum(nowMicros()) + `,"duration":5000,"status":"ok","attributes":{}}]}`
	if res := post(t, base+"/v1/events", body); res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if _, s := getJSON(t, base+"/api/search?q=debounced"); len(s["traces"].([]any)) != 1 {
		t.Errorf("search: %+v", s)
	}
	if _, d := getJSON(t, base+"/api/days"); len(d["days"].([]any)) != 1 {
		t.Errorf("days: %+v", d)
	}
}

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestRetentionPrunesOnStartup(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	w := core.NewTraceWindow(core.WindowOptions{})
	w.Add([]core.Event{{TraceID: "ancient", SpanID: "s", Participant: "gw", Operation: "GET /old", Kind: core.KindServer, StartTime: 1_000_000, Duration: 1000, Status: core.StatusOK, Attributes: core.Attributes{}}})
	seed, err := history.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.UpsertMany([]*core.AssembledTrace{w.Assemble("ancient")}); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	seven := 7
	_, base := start(t, Options{DBPath: dbPath, RetentionDays: &seven})
	if _, d := getJSON(t, base+"/api/days"); len(d["days"].([]any)) != 0 {
		t.Errorf("1970 day should be pruned at startup: %+v", d)
	}
}

func TestNativeEvents(t *testing.T) {
	_, base := start(t, Options{})
	res := post(t, base+"/v1/events", `{"events":[
	 {"traceId":"nt-1","spanId":"a","participant":"gw","operation":"GET /x","kind":"server","startTime":1000000,"duration":5000,"status":"ok","attributes":{}},
	 {"traceId":"nt-1","spanId":"b","parentSpanId":"a","participant":"billing","operation":"charge","kind":"client","startTime":1001000,"duration":2000,"status":"error","attributes":{"user.id":"42"}}]}`)
	var ack map[string]any
	_ = json.NewDecoder(res.Body).Decode(&ack)
	if res.StatusCode != 200 || ack["accepted"] != float64(2) {
		t.Fatalf("status %d ack %v", res.StatusCode, ack)
	}
	_, list := getJSON(t, base+"/api/traces")
	tr := list["traces"].([]any)
	if len(tr) != 1 || tr[0].(map[string]any)["traceId"] != "nt-1" || tr[0].(map[string]any)["hasError"] != true {
		t.Errorf("list: %+v", list)
	}
	if _, det := getJSON(t, base+"/api/traces/nt-1"); len(det["spans"].([]any)) != 2 {
		t.Errorf("detail spans: %+v", det["spans"])
	}
}

func TestPoisonTimestampCannotCrashFlush(t *testing.T) {
	_, base := start(t, Options{HistoryFlush: time.Minute})
	if res := post(t, base+"/v1/events", `{"events":[{"traceId":"poison","spanId":"poison","operation":"x","startTime":1e30,"duration":1}]}`); res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if s, _ := getJSON(t, base+"/api/traces"); s != 200 {
		t.Errorf("api status %d", s)
	}
	if s, _ := getJSON(t, base+"/healthz"); s != 200 {
		t.Errorf("healthz %d", s)
	}
}

func TestIngestScrubsPII(t *testing.T) {
	_, base := start(t, Options{})
	post(t, base+"/v1/events", `{"events":[{"traceId":"pii-1","spanId":"s","participant":"auth","operation":"POST /login","kind":"server",
	 "startTime":1700000000000000,"duration":1000,"status":"ok","attributes":{
	 "app.user.email":"victim@example.com","enduser.credit_card":"4242 4242 4242 4242","http.request.header.authorization":"Bearer abc.def.ghi"}}]}`)
	_, det := getJSON(t, base+"/api/traces/pii-1")
	attrs := det["spans"].([]any)[0].(map[string]any)["attributes"].(map[string]any)
	for _, k := range []string{"app.user.email", "enduser.credit_card", "http.request.header.authorization"} {
		if attrs[k] != "[redacted]" {
			t.Errorf("%s = %v", k, attrs[k])
		}
	}
}

func TestOversizedBodyAndGzipBomb(t *testing.T) {
	_, base := start(t, Options{})
	oversized := `{"events":[{"traceId":"x","spanId":"y","operation":"` + strings.Repeat("A", 12*1024*1024) + `"}]}`
	if res := post(t, base+"/v1/events", oversized); res.StatusCode != 413 {
		t.Errorf("oversized status %d", res.StatusCode)
	}

	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	_, _ = zw.Write(bytes.Repeat([]byte{65}, 64*1024*1024))
	zw.Close()
	if res := post(t, base+"/v1/traces", bomb.String(), "content-encoding", "gzip"); res.StatusCode < 400 {
		t.Errorf("gzip bomb status %d", res.StatusCode)
	}
	if s, _ := getJSON(t, base+"/healthz"); s != 200 {
		t.Error("server should survive the gzip bomb")
	}

	// A legitimate gzip payload still ingests.
	var ok bytes.Buffer
	zw = gzip.NewWriter(&ok)
	_, _ = zw.Write([]byte(otlpFixture))
	zw.Close()
	if res := post(t, base+"/v1/traces", ok.String(), "content-encoding", "gzip"); res.StatusCode != 200 {
		t.Errorf("gzip ingest status %d", res.StatusCode)
	}
}

func TestCORSReflectsLoopbackOnly(t *testing.T) {
	_, base := start(t, Options{})
	check := func(origin, want string) {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/traces", nil)
		req.Header.Set("origin", origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if got := res.Header.Get("access-control-allow-origin"); got != want {
			t.Errorf("origin %s: got %q want %q", origin, got, want)
		}
	}
	check("http://localhost:5173", "http://localhost:5173")
	check("https://evil.example", "")

	// Cross-site WebSocket is refused too.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("evil websocket should be refused: err=%v", err)
	}
}

type hookRecorder struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (h *hookRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	h.mu.Lock()
	h.calls = append(h.calls, body)
	h.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func (h *hookRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func TestAlertTransitionFiresWebhookOnce(t *testing.T) {
	rec := &hookRecorder{}
	hook := DefaultWebhookOptions("http://hook.test")
	hook.Client = &http.Client{Transport: rec}
	_, base := start(t, Options{TopologyInterval: 20 * time.Millisecond, AlertWebhook: &hook})

	// A service ("order") handling only failing requests → error rate 100% → an error alert.
	now := nowMicros()
	var events []string
	for i := range 4 {
		events = append(events, `{"traceId":"t`+jsonNum(int64(i))+`","spanId":"s`+jsonNum(int64(i))+`","participant":"order","operation":"POST /order","kind":"server","startTime":`+
			jsonNum(now+int64(i))+`,"duration":5000,"status":"error","attributes":{}}`)
	}
	if res := post(t, base+"/v1/events", "["+strings.Join(events, ",")+"]"); res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for rec.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if rec.count() != 1 {
		t.Fatalf("webhook calls: %d", rec.count())
	}
	alert := rec.calls[0]["alert"].(map[string]any)
	if rec.calls[0]["event"] != "alert.firing" || alert["id"] != "service:order:error-rate" || alert["severity"] != "error" {
		t.Errorf("payload: %+v", rec.calls[0])
	}
	time.Sleep(80 * time.Millisecond)
	if rec.count() != 1 {
		t.Error("no re-fire while the alert stays active")
	}
}

func TestServiceDetail(t *testing.T) {
	_, base := start(t, Options{})
	post(t, base+"/v1/traces", otlpFixture)
	_, d := getJSON(t, base+"/api/service/catalog")
	if d["name"] != "catalog" || d["spans"] != float64(3) || len(d["inbound"].([]any)) != 1 || len(d["outbound"].([]any)) != 2 {
		t.Errorf("service detail: %+v", d)
	}
}
