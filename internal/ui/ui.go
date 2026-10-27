// Package ui is LiveProbe's server-rendered web UI. Pages are templ components rendered from the
// same in-process state the API serves; live pages subscribe to /ui/live (Server-Sent Events),
// which pushes pre-rendered HTML fragments that a small vanilla client applies in one
// requestAnimationFrame batch. Layout, health colouring and SVG geometry all happen in Go
// (package view); the browser only swaps fragments and handles zoom/pan.
package ui

//go:generate go tool templ generate

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"

	"liveprobe/internal/core"
	"liveprobe/internal/history"
)

// Version is shown in the header and used to cache-bust static assets. Overridable via
// -ldflags "-X liveprobe/internal/ui.Version=…".
var Version = "0.1.0"

// LiveState is the live window's aggregate view: what the topology tick pushes.
type LiveState struct {
	Topology    core.Topology
	MermaidFlow string
	Red         []core.RedMetric
	Alerts      []core.FiringAlert
}

// Event is one live update: new/updated trace summaries, or a fresh LiveState.
type Event struct {
	Traces []core.TraceSummary
	Live   *LiveState
}

// Backend is what the UI needs from the server. The server implements it; ui never imports server.
type Backend interface {
	LiveState() LiveState
	RecentTraces(limit int) []core.TraceSummary
	// Subscribe streams Events; the channel closes if the consumer falls behind.
	Subscribe() (<-chan Event, func())
	TraceDetail(id string) (*core.TraceDetail, error)
	ServiceDetail(name string) core.ServiceDetail
	History() *history.Store
}

//go:embed static
var staticFS embed.FS

// Handler serves the UI pages, the /ui/live stream and /static assets.
func Handler(b Backend) http.Handler {
	static, _ := fs.Sub(staticFS, "static")
	files := http.StripPrefix("/static/", http.FileServer(http.FS(static)))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cache-control", "public, max-age=31536000, immutable") // URLs carry ?v=Version
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /ui/live", liveStream(b))
	mux.HandleFunc("GET /trace/{id}", tracePage(b))
	mux.HandleFunc("GET /alerts", alertsPage(b))
	mux.HandleFunc("GET /errors", errorsPage(b))
	mux.HandleFunc("GET /search", searchPage(b))
	mux.HandleFunc("GET /history", historyPage(b))
	mux.HandleFunc("GET /day/{date}", dayPage(b))
	mux.HandleFunc("GET /service/{name}", servicePage(b))
	mux.HandleFunc("GET /compare", comparePage(b))
	mux.HandleFunc("GET /ui/trace/{id}/span/{span}", spanFragment(b))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, livePage(b, liveParamsFrom(r.URL.Query())))
	})
	// Unknown routes go home, as the React app's catch-all <Navigate to="/"> did.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	})
	return mux
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// renderString renders a component to a string (for SSE fragments).
func renderString(ctx context.Context, c templ.Component) string {
	var b strings.Builder
	_ = c.Render(ctx, &b)
	return b.String()
}

func asset(name string) string { return "/static/" + name + "?v=" + url.QueryEscape(Version) }

// --- live view parameters (kept in the URL so views are shareable) -------------------------

type liveParams struct {
	Service string // filter: only this service and its direct neighbours
	View    string // "graph" | "matrix"
	Metric  string // matrix cell metric: "calls" | "errors" | "latency"
}

func liveParamsFrom(q url.Values) liveParams {
	p := liveParams{Service: q.Get("service"), View: q.Get("view"), Metric: q.Get("metric")}
	if p.View != "matrix" {
		p.View = "graph"
	}
	if p.Metric != "errors" && p.Metric != "latency" {
		p.Metric = "calls"
	}
	return p
}

// href builds a live-page URL with some parameters changed.
func (p liveParams) href(change func(*liveParams)) string {
	c := p
	change(&c)
	q := url.Values{}
	if c.Service != "" {
		q.Set("service", c.Service)
	}
	if c.View != "graph" {
		q.Set("view", c.View)
	}
	if c.Metric != "calls" {
		q.Set("metric", c.Metric)
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

// streamURL is the /ui/live subscription for this view.
func (p liveParams) streamURL() string {
	q := url.Values{"page": {"live"}, "view": {p.View}, "metric": {p.Metric}}
	if p.Service != "" {
		q.Set("service", p.Service)
	}
	return "/ui/live?" + q.Encode()
}

func serviceHref(name string) string { return "/service/" + url.PathEscape(name) }
func traceHref(id string) string     { return "/trace/" + url.PathEscape(id) }
