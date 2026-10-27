package ui

import (
	"strconv"
	"testing"

	"liveprobe/internal/core"
	"liveprobe/internal/history"
)

// Formatting helpers: expected values captured from Node (the React UI's runtime).

func TestFlameColorMatchesJS(t *testing.T) {
	cases := map[string]string{"gateway": "flame-c0", "catalog": "flame-c5", "postgresql": "flame-c4", "hrms.roles": "flame-c5",
		"a-very-long-participant-name-overflowing-int32": "flame-c3", "é": "flame-c5"}
	for in, want := range cases {
		if got := flameColor(in); got != want {
			t.Errorf("flameColor(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestFiringFor(t *testing.T) {
	cases := map[int64]string{0: "0s", 499: "0s", 500: "1s", 59499: "59s", 59500: "1m 0s", 61000: "1m 1s",
		3599000: "59m 59s", 3600000: "1h 0m", 7505000: "2h 5m", -5000: "0s"}
	for elapsed, want := range cases {
		if got := firingFor(0, elapsed); got != want {
			t.Errorf("firingFor(%d) = %q, want %q", elapsed, got, want)
		}
	}
}

func TestGrouped(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1000: "1,000", 123456: "123,456", -1234567: "-1,234,567", 1000000: "1,000,000"}
	for in, want := range cases {
		if got := grouped(in); got != want {
			t.Errorf("grouped(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareDiffOrdersByLargestChange(t *testing.T) {
	a := &core.TraceDetail{Spans: []core.SpanRow{{Participant: "gw", Operation: "GET /", Duration: 100}, {Participant: "db", Operation: "q", Duration: 10}}}
	b := &core.TraceDetail{Spans: []core.SpanRow{{Participant: "gw", Operation: "GET /", Duration: 90}, {Participant: "db", Operation: "q", Duration: 60}, {Participant: "new", Operation: "x", Duration: 5}}}
	rows := compareDiff(a, b)
	keys := []string{rows[0].Key, rows[1].Key, rows[2].Key}
	if keys[0] != "db: q" || keys[1] != "gw: GET /" || keys[2] != "new: x" {
		t.Errorf("order: %v", keys)
	}
	if rows[0].Delta != "+50µs" || rows[0].DeltaClass != "num err-text" || rows[1].Delta != "−10µs" || rows[2].A != "—" {
		t.Errorf("rows: %+v", rows)
	}
}

// historyBackend is fixtureBackend plus a real in-memory history store with two days of traces.
func historyBackend(t *testing.T) *storeBackend {
	t.Helper()
	store, err := history.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	const day = int64(86_400_000_000)
	var traces []*core.AssembledTrace
	for i := range 30 {
		status := core.StatusOK
		if i%5 == 0 {
			status = core.StatusError
		}
		start := 20*day + int64(i)*90_000_000 // 1970-01-21, a request every 90s
		events := []core.Event{
			{TraceID: "t" + itoa(i), SpanID: "r", Participant: "gateway", Operation: "POST /checkout", Kind: core.KindServer, StartTime: start, Duration: int64(10_000 + i*1_000), Status: status, Attributes: core.Attributes{"http.status_code": float64(500)}},
			{TraceID: "t" + itoa(i), SpanID: "c", ParentSpanID: "r", Participant: "order", Operation: "INSERT", Kind: core.KindClient, Peer: "postgresql", StartTime: start + 1_000, Duration: 4_000, Status: core.StatusOK, Attributes: core.Attributes{}},
		}
		traces = append(traces, core.AssembleEvents("t"+itoa(i), events))
	}
	if err := store.UpsertMany(traces); err != nil {
		t.Fatal(err)
	}
	b := &storeBackend{fakeBackend: fixtureBackend(), store: store}
	b.details = map[string]core.TraceDetail{"t0": core.Detail(traces[0]), "t1": core.Detail(traces[1])}
	return b
}

func itoa(n int) string { return strconv.Itoa(n) }

type storeBackend struct {
	*fakeBackend
	store *history.Store
}

func (s *storeBackend) History() *history.Store { return s.store }

func TestHistoryAndDayPages(t *testing.T) {
	h := Handler(historyBackend(t))
	_, body := get(t, h, "/history")
	mustContain(t, body, `href="/day/1970-01-21"`, `class="trend-bar"`, `class="trend-bar-err"`, "1970-01-21: 30 requests, 6 errors", `class="nav-link nav-link-active">History`)

	_, body = get(t, h, "/day/1970-01-21")
	mustContain(t, body, ">30<", "20.0%", `class="chart-bar"`, "Click an endpoint above",
		`data-href="/day/1970-01-21?endpoint=POST+%2Fcheckout#latency"`, "Slowest traces")

	_, body = get(t, h, "/day/1970-01-21?endpoint=POST+%2Fcheckout")
	mustContain(t, body, `class="row-clickable row-selected"`, `class="lat-line lat-p99"`, `class="lat-hist-bar"`, `class="heat-cell"`,
		`class="flame-cell"`, "Where time goes — POST /checkout", `id="latency"`)
}

func TestErrorsSearchServiceComparePages(t *testing.T) {
	h := Handler(historyBackend(t))
	_, body := get(t, h, "/errors")
	mustContain(t, body, "gateway: HTTP 500", `href="/trace/t25"`, "UTC") // the newest errored trace is the group's sample

	_, body = get(t, h, "/search")
	mustContain(t, body, "Enter a filter above")
	_, body = get(t, h, "/search?q=checkout&error=true")
	mustContain(t, body, "6 results", `<option value="true" selected>`, `value="checkout"`)
	_, body = get(t, h, "/search?q=nothing-matches")
	mustContain(t, body, "0 results", "No traces match those filters.")

	_, body = get(t, h, "/service/catalog")
	mustContain(t, body, `<h1 class="page-title mono">catalog</h1>`, "Spans (live window)")

	_, body = get(t, h, "/compare?a=t0&b=t1")
	mustContain(t, body, `class="cmp-badge">A`, `class="cmp-badge">B`, "gateway: POST /checkout", "+1.0ms")
	_, body = get(t, h, "/compare?a=nope")
	mustContain(t, body, "Trace A not found.")
}

func TestAlertsPageAndStream(t *testing.T) {
	b := fixtureBackend()
	b.state.Alerts = []core.FiringAlert{{Alert: core.Alert{ID: "service:order:error-rate", Kind: "service", Target: "order", Metric: "error-rate", Severity: "error", Message: "order error rate 14% (14/100)"}, Since: 1}}
	_, body := get(t, Handler(b), "/alerts")
	mustContain(t, body, "alert-row alert-row-error", "order error rate 14% (14/100)", `data-since="1"`, `href="/service/order"`, `data-stream="/ui/live?page=alerts"`)
	_, body = get(t, Handler(&fakeBackend{}), "/alerts")
	mustContain(t, body, "No active alerts")
}
