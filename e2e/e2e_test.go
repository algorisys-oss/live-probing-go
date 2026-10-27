// Package e2e drives the server-rendered UI in a real (headless) Chrome via chromedp. It is the
// Go port of ../live-probing/e2e/*.spec.ts (same seed fixtures, same assertions) plus the
// behaviours the Go UI adds: live streaming, pause, zoom persistence, URL-driven views.
//
// Skipped under -short or when no Chrome/Chromium binary is found (set LIVEPROBE_CHROME to point
// at one explicitly).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"liveprobe/internal/core"
	"liveprobe/internal/server"
)

var (
	baseURL  string
	allocCtx context.Context
	skipWhy  string
)

func findChrome() string {
	if p := os.Getenv("LIVEPROBE_CHROME"); p != "" {
		return p
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "/opt/google/chrome/chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func TestMain(m *testing.M) {
	// testing.Short() needs flags parsed.
	if fs := flagShort(); fs {
		skipWhy = "-short"
		os.Exit(m.Run())
	}
	chrome := findChrome()
	if chrome == "" {
		skipWhy = "no Chrome/Chromium found (set LIVEPROBE_CHROME)"
		os.Exit(m.Run())
	}
	srv, err := server.New(server.Options{DBPath: ":memory:"})
	if err != nil {
		panic(err)
	}
	ts := httptest.NewServer(srv.Handler())
	baseURL = ts.URL

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome), chromedp.WindowSize(1400, 900))
	var cancel context.CancelFunc
	allocCtx, cancel = chromedp.NewExecAllocator(context.Background(), opts...)
	code := m.Run()
	cancel()
	ts.Close()
	srv.Close()
	os.Exit(code)
}

// --- seed (port of e2e/seed.ts) --------------------------------------------------------------

const ms = 1000 // microseconds per millisecond

var ops = struct{ checkout, cart, catalog string }{"POST /checkout", "GET /cart", "GET /catalog"}

const (
	checkoutID = "e2e-checkout"
	cartID     = "e2e-cart"
	catalogID  = "e2e-catalog"
	ghostID    = "e2e-ghost"
	nplusoneID = "e2e-nplusone"
	gateway    = "web-gateway"
)

func ev(trace, span, parent, who, peer, op string, kind core.SpanKind, start, dur int64, status core.SpanStatus, attrs core.Attributes) core.Event {
	if attrs == nil {
		attrs = core.Attributes{}
	}
	return core.Event{TraceID: trace, SpanID: span, ParentSpanID: parent, Participant: who, Peer: peer, Operation: op,
		Kind: kind, StartTime: start, Duration: dur, Status: status, Attributes: attrs}
}

// crossService: gateway root server span → outbound client span (peer) → downstream server span.
func crossService(id, rootOp, downstream, downstreamOp string, start int64, failed bool) []core.Event {
	status, attrs := core.StatusOK, core.Attributes{"http.status_code": float64(200)}
	if failed {
		status, attrs = core.StatusError, core.Attributes{"error.type": "PaymentDeclined", "http.status_code": float64(402)}
	}
	return []core.Event{
		ev(id, id+"-a", "", gateway, "", rootOp, core.KindServer, start, 60*ms, core.StatusOK, core.Attributes{"http.route": rootOp}),
		ev(id, id+"-b", id+"-a", gateway, downstream, rootOp, core.KindClient, start+2*ms, 50*ms, status, nil),
		ev(id, id+"-c", id+"-b", downstream, "", downstreamOp, core.KindServer, start+3*ms, 45*ms, status, attrs),
	}
}

func seedEvents(now int64) []core.Event {
	events := append(crossService(checkoutID, ops.checkout, "payments", "charge", now-3_000_000, true),
		crossService(cartID, ops.cart, "cart-service", "read", now-2_000_000, false)...)
	events = append(events, crossService(catalogID, ops.catalog, "catalog", "list", now-1_000_000, false)...)
	// An uninstrumented peer: the client span's peer never reports, so it renders as a ghost node.
	events = append(events,
		ev(ghostID, ghostID+"-a", "", gateway, "", "GET /legacy", core.KindServer, now-500_000, 30*ms, core.StatusOK, core.Attributes{"http.route": "GET /legacy"}),
		ev(ghostID, ghostID+"-b", ghostID+"-a", gateway, "legacy-api", "GET /legacy", core.KindClient, now-498_000, 25*ms, core.StatusOK, nil),
	)
	// Go-UI extra: an N+1 (5 sequential identical queries) to exercise sequence-group expansion.
	start := now - 4_000_000
	events = append(events, ev(nplusoneID, "r", "", "orders", "", "GET /orders", core.KindServer, start, 40*ms, core.StatusOK, nil))
	for i := range 5 {
		events = append(events, ev(nplusoneID, fmt.Sprintf("q%d", i), "r", "orders", "postgresql", "SELECT item",
			core.KindClient, start+int64(1+i*7)*ms, 5*ms, core.StatusOK, core.Attributes{"db.system": "postgresql"}))
	}
	return events
}

func post(t *testing.T, events []core.Event) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"events": events})
	res, err := http.Post(baseURL+"/v1/events", "application/json", bytes.NewReader(body))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("ingest failed: %v %v", err, res)
	}
	res.Body.Close()
}

var seedOnce sync.Once

func seed(t *testing.T) {
	t.Helper()
	seedOnce.Do(func() { post(t, seedEvents(time.Now().UnixMilli()*1000)) })
}

// --- a tiny browser helper (chromedp v0.21: Do / Run / typed Evaluate) ----------------------

type browser struct {
	t      *testing.T
	ctx    context.Context
	mu     sync.Mutex
	errors []string
}

func open(t *testing.T) *browser {
	t.Helper()
	if skipWhy != "" {
		t.Skip("e2e skipped: " + skipWhy)
	}
	seed(t)
	ctx, cancel := chromedp.NewContext(allocCtx)
	ctx, cancelTimeout := context.WithTimeout(ctx, 45*time.Second)
	b := &browser{t: t, ctx: ctx}
	if err := chromedp.Do(ctx, chromedp.Navigate("about:blank")); err != nil { // start the tab
		t.Fatalf("browser: %v", err)
	}
	go func() {
		for m, err := range chromedp.Console(ctx) {
			if err != nil {
				return
			}
			// A deliberate 404 (unknown trace) is logged as a resource error; anything else fails.
			if (m.Type == chromedp.ConsoleError || m.Type == chromedp.ConsoleException) && !strings.Contains(m.Text, "404") {
				b.mu.Lock()
				b.errors = append(b.errors, string(m.Type)+": "+m.Text)
				b.mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() {
		b.mu.Lock()
		errs := b.errors
		b.mu.Unlock()
		if len(errs) > 0 {
			t.Errorf("page errors: %v", errs)
		}
		cancelTimeout()
		cancel()
	})
	return b
}

func (b *browser) run(actions ...chromedp.Action[chromedp.Void]) {
	b.t.Helper()
	if err := chromedp.Do(b.ctx, actions...); err != nil {
		b.t.Fatalf("browser: %v", err)
	}
}

func (b *browser) goTo(path string) {
	b.t.Helper()
	b.run(chromedp.Navigate(baseURL + path))
}

func eval[T any](b *browser, expr string) T {
	b.t.Helper()
	v, err := chromedp.Run(b.ctx, chromedp.Evaluate[T](expr))
	if err != nil {
		b.t.Fatalf("eval %s: %v", expr, err)
	}
	return v
}

func (b *browser) exec(expr string)            { b.t.Helper(); b.run(chromedp.Evaluate[chromedp.Void](expr)) }
func (b *browser) jsInt(expr string) int       { b.t.Helper(); return eval[int](b, expr) }
func (b *browser) jsString(expr string) string { b.t.Helper(); return eval[string](b, expr) }

// waitJS polls a boolean expression until it is true (the auto-retrying expect of Playwright).
func (b *browser) waitJS(what, expr string) {
	b.t.Helper()
	deadline := time.Now().Add(7 * time.Second)
	for {
		if ok, err := chromedp.Run(b.ctx, chromedp.Evaluate[bool]("!!("+expr+")")); err == nil && ok {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out waiting for %s (%s)", what, expr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// count/visible/text are JS snippets over a CSS selector.
func sel(css string) string { b, _ := json.Marshal(css); return string(b) }
func count(css string) string {
	return "document.querySelectorAll(" + sel(css) + ").length"
}
func visibleCount(css string) string {
	return "[...document.querySelectorAll(" + sel(css) + ")].filter(e => e.checkVisibility()).length"
}
func textOf(css string) string {
	return "(document.querySelector(" + sel(css) + ")||{}).textContent||''"
}

// withText finds the first element matching css whose text contains s (Playwright's hasText).
func withText(css, s string) string {
	q, _ := json.Marshal(s)
	return "[...document.querySelectorAll(" + sel(css) + ")].find(e => e.textContent.includes(" + string(q) + "))"
}

func (b *browser) click(css string) {
	b.t.Helper()
	b.run(chromedp.Click(chromedp.CSS(css)))
}

// clickJS clicks the element an expression yields (for text-based lookups).
func (b *browser) clickJS(what, expr string) {
	b.t.Helper()
	b.waitJS(what, expr)
	// dispatchEvent, not .click(): SVG elements (sequence arrows, graph nodes) have no click().
	b.exec("(() => { const e = " + expr + "; e.scrollIntoView(); e.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true, view: window})) })()")
}

func (b *browser) url() string {
	b.t.Helper()
	u, err := chromedp.Run(b.ctx, chromedp.Location())
	if err != nil {
		b.t.Fatalf("location: %v", err)
	}
	return u
}

func (b *browser) waitURL(what string, pred func(string) bool) string {
	b.t.Helper()
	deadline := time.Now().Add(7 * time.Second)
	for {
		u := b.url()
		if pred(u) {
			return u
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out waiting for URL %s (at %s)", what, u)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// flagShort reads -test.short before flag.Parse has run in TestMain.
func flagShort() bool {
	for _, a := range os.Args[1:] {
		if a == "-test.short" || a == "-test.short=true" {
			return true
		}
	}
	return false
}
