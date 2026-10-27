package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"liveprobe/internal/core"
)

// The /ui/live stream. Every connection starts with a full "reset" (so an EventSource reconnect
// self-heals), then pushes "traces" (new/updated rows) and "live" (header + view fragments)
// rendered for this client's view parameters. If the client falls behind, the server drops the
// subscription and the stream ends; EventSource reconnects and gets a fresh reset.

type rowFragment struct {
	ID    string `json:"id"`
	Start int64  `json:"start"`
	HTML  string `json:"html"`
}

type liveFragments struct {
	Services      int    `json:"services"`
	Traces        int    `json:"traces"`
	Alerts        int    `json:"alerts"`
	AlertSeverity string `json:"alertSeverity"`
	// Live page only (empty elsewhere):
	Red           string `json:"red,omitempty"`
	FilterOptions string `json:"filterOptions,omitempty"`
	Flow          string `json:"flow,omitempty"` // inner content of #lp-flow-content
	FlowFit       string `json:"flowFit,omitempty"`
	FlowEmpty     bool   `json:"flowEmpty"`
	Mermaid       string `json:"mermaid,omitempty"`
	Matrix        string `json:"matrix,omitempty"`
	// Alerts page only:
	AlertList string `json:"alertList,omitempty"`
}

const maxListedTraces = 200 // the live list's cap, as in the React store

func rows(ctx context.Context, ts []core.TraceSummary, service string) []rowFragment {
	out := []rowFragment{}
	for _, t := range ts {
		if traceTouches(t, service) {
			out = append(out, rowFragment{ID: t.TraceID, Start: t.StartTime, HTML: renderString(ctx, traceRow(t))})
		}
	}
	return out
}

func fragments(ctx context.Context, st LiveState, p liveParams, page string, traceCount int) liveFragments {
	f := liveFragments{Services: len(st.Topology.Nodes), Traces: traceCount, Alerts: len(st.Alerts), AlertSeverity: alertSeverity(st.Alerts)}
	if page == "alerts" {
		f.AlertList = renderString(ctx, alertList(alertRows(st.Alerts, time.Now().UnixMilli())))
	}
	if page != "live" {
		return f
	}
	f.Red = renderString(ctx, redTiles(st.Red))
	f.FilterOptions = renderString(ctx, filterOptions(st.Topology.Nodes, p.Service))
	if p.View == "matrix" {
		f.Matrix = renderString(ctx, matrixContent(buildMatrix(st.Topology, p), p.Metric))
	} else {
		g := buildFlow(st.Topology, p.Service)
		f.Flow, f.FlowFit, f.FlowEmpty, f.Mermaid = renderString(ctx, flowContent(g)), g.Fit, g.Empty, st.MermaidFlow
	}
	return f
}

func liveStream(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		q := r.URL.Query()
		p, page := liveParamsFrom(q), q.Get("page")
		onLive := page == "live"
		ctx := r.Context()

		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		w.Header().Set("x-accel-buffering", "no") // don't let a proxy buffer the stream
		send := func(event string, v any) bool {
			data, err := json.Marshal(v)
			if err != nil {
				return true
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}

		// Subscribe before reading state so nothing published in between is lost.
		events, cancel := b.Subscribe()
		defer cancel()

		recent := b.RecentTraces(maxListedTraces)
		traceCount := len(recent)
		reset := map[string]any{"live": fragments(ctx, b.LiveState(), p, page, traceCount)}
		if onLive {
			reset["rows"] = rows(ctx, recent[:min(100, len(recent))], p.Service)
		}
		if !send("reset", reset) {
			return
		}

		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()
		seen := map[string]bool{}
		for _, t := range recent {
			seen[t.TraceID] = true
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-keepalive.C:
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case ev, ok := <-events:
				if !ok {
					return // dropped for falling behind; the client reconnects and resets
				}
				if ev.Traces != nil {
					for _, t := range ev.Traces {
						if !seen[t.TraceID] && traceCount < maxListedTraces {
							seen[t.TraceID] = true
							traceCount++
						}
					}
					if onLive {
						if rs := rows(ctx, ev.Traces, p.Service); len(rs) > 0 && !send("traces", map[string]any{"rows": rs, "count": traceCount}) {
							return
						}
					}
				}
				if ev.Live != nil && !send("live", fragments(ctx, *ev.Live, p, page, traceCount)) {
					return
				}
			}
		}
	}
}
