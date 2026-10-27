// Package server is LiveProbe's HTTP + WebSocket server: OTLP and native ingest, the REST API
// over the live window and the history store, the live /ws stream, and alert evaluation.
// The wire format (JSON on /v1, /api and /ws) is identical to the TS server.
package server

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"liveprobe/internal/core"
	"liveprobe/internal/history"
)

type Options struct {
	PublicDir     string // static UI to serve, if any
	DBPath        string // sqlite history db; ":memory:" (default) or a file path
	MaxTraces     int    // live window cap; default 2000
	HorizonMicros int64  // live window horizon; default 5 min
	// Topology/RED/alerts push cadence; default 750ms.
	TopologyInterval time.Duration
	// Keep the N most recent UTC days of history (including today); older days are pruned at
	// startup and hourly. nil = 30; 0 disables.
	RetentionDays *int
	// How often staged history writes are flushed. Any /api read flushes first
	// (read-your-writes), so this bounds crash loss, not visibility. Default 1500ms.
	HistoryFlush time.Duration
	// Interface to bind in ListenAndServe. Default "127.0.0.1" — the ingest and API have no auth.
	Host string
	// PII/secret masking at ingest. nil = built-in ruleset; MaskDisabled turns it off.
	Mask         *core.MaskOptions
	MaskDisabled bool
	// Extra browser origins allowed for CORS / WebSocket beyond loopback (any port).
	CORSOrigins []string
	// Outbound webhook fired once per inactive→active alert transition. nil disables.
	AlertWebhook *WebhookOptions
}

// Hard limits on one ingest request: the body cap bounds buffering; the decompression cap bounds
// a gzip bomb.
const (
	maxBodyBytes         = 8 * 1024 * 1024
	maxDecompressedBytes = 32 * 1024 * 1024
)

type Server struct {
	opts   Options
	window *core.TraceWindow
	store  *history.Store
	mask   *core.MaskOptions

	clientsMu sync.Mutex
	clients   map[*client]bool

	topologyDirty atomic.Bool

	pendingMu sync.Mutex
	pending   map[string]*core.AssembledTrace

	alertsMu    sync.Mutex
	activeSince map[string]int64
	notifier    *WebhookNotifier

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	http   *http.Server
	closed sync.Once
}

func New(o Options) (*Server, error) {
	if o.MaxTraces == 0 {
		o.MaxTraces = 2000
	}
	if o.TopologyInterval == 0 {
		o.TopologyInterval = 750 * time.Millisecond
	}
	if o.HistoryFlush == 0 {
		o.HistoryFlush = 1500 * time.Millisecond
	}
	if o.Host == "" {
		o.Host = "127.0.0.1"
	}
	store, err := history.Open(o.DBPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		opts:        o,
		window:      core.NewTraceWindow(core.WindowOptions{MaxTraces: o.MaxTraces, HorizonMicros: o.HorizonMicros}),
		store:       store,
		clients:     map[*client]bool{},
		pending:     map[string]*core.AssembledTrace{},
		activeSince: map[string]int64{},
		ctx:         ctx,
		cancel:      cancel,
	}
	if !o.MaskDisabled {
		s.mask = &core.MaskOptions{}
		if o.Mask != nil {
			s.mask = o.Mask
		}
	}
	if o.AlertWebhook != nil && o.AlertWebhook.URL != "" {
		s.notifier = NewWebhookNotifier(*o.AlertWebhook)
	}

	retention := 30
	if o.RetentionDays != nil {
		retention = *o.RetentionDays
	}
	prune := func() {
		dropped, err := s.store.Prune(retention, time.Now().UnixMilli()*1000)
		if err != nil {
			log.Printf("[liveprobe] retention prune failed: %v", err)
		} else if dropped > 0 {
			log.Printf("[liveprobe] retention: pruned %d traces older than %d days", dropped, retention)
		}
	}
	prune()
	s.every(time.Hour, prune)
	s.every(o.HistoryFlush, s.flushHistory)
	s.every(o.TopologyInterval, s.tick)
	return s, nil
}

func (s *Server) every(d time.Duration, f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				f()
			}
		}
	}()
}

func (s *Server) Window() *core.TraceWindow { return s.window }

// ListenAndServe binds Options.Host:port and serves until Close.
func (s *Server) ListenAndServe(port int) error {
	s.http = &http.Server{Addr: net.JoinHostPort(s.opts.Host, strconv.Itoa(port)), Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the timers, disconnects live clients, flushes staged history and closes the store.
func (s *Server) Close() error {
	var err error
	s.closed.Do(func() {
		s.cancel()
		s.wg.Wait()
		s.clientsMu.Lock()
		for c := range s.clients {
			delete(s.clients, c) // only clients still in the map are ever sent to
			c.close()
		}
		s.clientsMu.Unlock()
		if s.http != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.http.Shutdown(ctx)
			cancel()
		}
		s.flushHistory()
		err = s.store.Close()
	})
	return err
}

// --- history staging -----------------------------------------------------------------------

// flushHistory writes the latest staged assembly of each touched trace. A trace that grows across
// many ingest batches is written once per flush instead of once per batch. Never panics or
// returns an error: it runs on a timer with no caller to handle one.
func (s *Server) flushHistory() {
	s.pendingMu.Lock()
	if len(s.pending) == 0 {
		s.pendingMu.Unlock()
		return
	}
	batch := make([]*core.AssembledTrace, 0, len(s.pending))
	for _, t := range s.pending {
		batch = append(batch, t)
	}
	clear(s.pending)
	s.pendingMu.Unlock()
	if err := s.store.UpsertMany(batch); err != nil {
		log.Printf("[liveprobe] history flush failed: %v", err)
	}
}

// --- ingest --------------------------------------------------------------------------------

func (s *Server) ingest(raw []core.Event) {
	if len(raw) == 0 {
		return
	}
	events := raw
	if s.mask != nil {
		events = core.MaskEvents(raw, *s.mask)
	}
	s.window.Add(events)

	seen := map[string]bool{}
	var summaries []core.TraceSummary
	for _, e := range events {
		if seen[e.TraceID] {
			continue
		}
		seen[e.TraceID] = true
		t := s.window.Assemble(e.TraceID)
		if t == nil {
			continue
		}
		summaries = append(summaries, core.Summarize(t))
		s.pendingMu.Lock()
		s.pending[t.TraceID] = t // staged for the daily/history view
		s.pendingMu.Unlock()
	}
	if len(summaries) > 0 {
		s.broadcast(map[string]any{"type": "traces", "traces": summaries})
	}
	s.topologyDirty.Store(true)
}

// --- live projections ----------------------------------------------------------------------

func (s *Server) recentSummaries(limit float64) []core.TraceSummary {
	all := s.window.AssembleAll()
	out := make([]core.TraceSummary, len(all))
	for i, t := range all {
		out[i] = core.Summarize(t)
	}
	slices.SortStableFunc(out, func(a, b core.TraceSummary) int { return cmp.Compare(b.StartTime, a.StartTime) })
	return out[:jsSliceEnd(limit, len(out))]
}

// jsSliceEnd mirrors Array.prototype.slice(0, n) end-index semantics (negative counts from the end).
func jsSliceEnd(n float64, length int) int {
	if math.IsNaN(n) {
		return 0
	}
	end := int(math.Trunc(n))
	if n > float64(length) {
		end = length
	}
	if end < 0 {
		end = max(0, length+end)
	}
	return min(end, length)
}

// computeAlerts evaluates alerts and stamps each with a persistent first-seen time, so the UI can
// show "firing for Xs". A brand-new id is an inactive→active transition: the webhook fires in the
// background (never stalls the tick). Cleared ids drop out.
func (s *Server) computeAlerts(edges []core.Edge, red []core.RedMetric) []FiringAlert {
	fired := core.EvaluateAlerts(red, edges, core.DefaultAlertThresholds)
	now := time.Now().UnixMilli()
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	seen := map[string]bool{}
	out := make([]FiringAlert, 0, len(fired))
	for _, a := range fired {
		seen[a.ID] = true
		since, ok := s.activeSince[a.ID]
		if !ok {
			since = now
			s.activeSince[a.ID] = since
			if s.notifier != nil {
				fa := FiringAlert{Alert: a, Since: since}
				go s.notifier.Notify(fa)
			}
		}
		out = append(out, FiringAlert{Alert: a, Since: since})
	}
	for id := range s.activeSince {
		if !seen[id] {
			delete(s.activeSince, id)
		}
	}
	rank := map[string]int{"error": 0, "warn": 1}
	slices.SortStableFunc(out, func(a, b FiringAlert) int {
		return cmp.Or(cmp.Compare(rank[a.Severity], rank[b.Severity]), cmp.Compare(a.Since, b.Since))
	})
	return out
}

// liveState is the topology + RED + alerts payload shared by the snapshot and the tick.
func (s *Server) liveState(msgType string) map[string]any {
	topology := s.window.Topology()
	red := core.RedMetrics(s.window.AssembleAll())
	return map[string]any{
		"type":        msgType,
		"topology":    topology,
		"mermaidFlow": core.ToMermaidFlow(topology),
		"red":         red,
		"alerts":      s.computeAlerts(topology.Edges, red),
	}
}

func (s *Server) tick() {
	if !s.topologyDirty.Swap(false) {
		return
	}
	s.broadcast(s.liveState("topology"))
}

type operationStat struct {
	Operation string `json:"operation"`
	Calls     int    `json:"calls"`
	Errors    int    `json:"errors"`
	AvgMicros int64  `json:"avgMicros"`
}

// serviceDetail is the per-service view from the live window: deps (from the topology graph) and
// the service's own operations and error rate.
func (s *Server) serviceDetail(name string) map[string]any {
	topology := s.window.Topology()
	inbound, outbound := []string{}, []string{}
	for _, e := range topology.Edges {
		if e.To == name && !slices.Contains(inbound, e.From) {
			inbound = append(inbound, e.From)
		}
		if e.From == name && !slices.Contains(outbound, e.To) {
			outbound = append(outbound, e.To)
		}
	}
	type acc struct {
		calls, errors int
		dur           int64
	}
	ops := map[string]*acc{}
	var order []string
	spans, errs := 0, 0
	for _, t := range s.window.AssembleAll() {
		for _, e := range t.Events {
			if e.Participant != name {
				continue
			}
			spans++
			o := ops[e.Operation]
			if o == nil {
				o = &acc{}
				ops[e.Operation] = o
				order = append(order, e.Operation)
			}
			o.calls++
			o.dur += e.Duration
			if e.Status == core.StatusError {
				errs++
				o.errors++
			}
		}
	}
	operations := make([]operationStat, 0, len(order))
	for _, op := range order {
		o := ops[op]
		operations = append(operations, operationStat{op, o.calls, o.errors, int64(math.Floor(float64(o.dur)/float64(o.calls) + 0.5))})
	}
	slices.SortStableFunc(operations, func(a, b operationStat) int { return cmp.Compare(b.Calls, a.Calls) })
	operations = operations[:min(30, len(operations))]
	rate := 0.0
	if spans > 0 {
		rate = float64(errs) / float64(spans)
	}
	return map[string]any{"name": name, "spans": spans, "errors": errs, "errorRate": rate, "inbound": inbound, "outbound": outbound, "operations": operations}
}

// --- websocket -----------------------------------------------------------------------------

type client struct {
	conn *websocket.Conn
	send chan []byte
	once sync.Once
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.send)
		c.conn.Close(websocket.StatusGoingAway, "")
	})
}

const clientBuffer = 256

func (s *Server) broadcast(msg any) {
	data, err := marshal(msg)
	if err != nil {
		return
	}
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	for c := range s.clients {
		select {
		case c.send <- data:
		default: // a client that can't keep up is dropped rather than buffered without bound
			delete(s.clients, c)
			c.close()
		}
	}
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r.Header.Get("Origin")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// The origin was checked above (loopback or configured), so skip the library's same-host check.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(64 * 1024)
	c := &client{conn: conn, send: make(chan []byte, clientBuffer)}

	// Snapshot and registration happen under the lock so no broadcast slips in between: the
	// snapshot is always the first message and nothing after it is missed.
	s.clientsMu.Lock()
	snap := s.liveState("snapshot")
	snap["traces"] = s.recentSummaries(100)
	if data, err := marshal(snap); err == nil {
		c.send <- data
	}
	s.clients[c] = true
	s.clientsMu.Unlock()

	ctx := conn.CloseRead(s.ctx) // we never read app messages; this services control frames
	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, c)
		s.clientsMu.Unlock()
		c.close()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-c.send:
			if !ok {
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// --- http ----------------------------------------------------------------------------------

// originAllowed: a browser Origin is allowed if it is loopback (any port) or configured.
// Non-browser clients (no Origin) pass through; the check stops a malicious *website* from
// reading traces cross-origin or hijacking the live stream.
func (s *Server) originAllowed(origin string) bool {
	if origin == "" || slices.Contains(s.opts.CORSOrigins, origin) {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

var (
	dayRoute  = regexp.MustCompile(`^/api/day/([^/]+)/(latency|flamegraph|summary|traces)$`)
	errTooBig = errors.New("payload too large")
)

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reflect only allowed origins rather than "*", so an arbitrary website can't read the
		// trace store from the operator's browser.
		if origin := r.Header.Get("Origin"); origin != "" && s.originAllowed(origin) {
			w.Header().Set("access-control-allow-origin", origin)
			w.Header().Set("vary", "origin")
			w.Header().Set("access-control-allow-headers", "content-type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		path := r.URL.EscapedPath()
		if r.Method == http.MethodGet && path == "/ws" {
			s.serveWS(w, r)
			return
		}
		// Read-your-writes: API reads hit the history store, so land staged writes first.
		if r.Method == http.MethodGet && strings.HasPrefix(path, "/api/") {
			s.flushHistory()
		}
		if err := s.route(w, r, path); err != nil {
			// Never echo internal error text (it can leak paths/state).
			if errors.Is(err, errTooBig) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload_too_large"})
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
			}
		}
	})
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, path string) error {
	q := r.URL.Query()
	get := r.Method == http.MethodGet

	switch {
	case r.Method == http.MethodPost && (path == "/v1/traces" || path == "/v1/events"):
		body, err := readBody(w, r)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			body = []byte("{}")
		}
		// /v1/traces = OTLP spans; /v1/events = already-normalized events (from an adapter).
		if path == "/v1/traces" {
			p, err := core.DecodeOTLP(body)
			if err != nil {
				return err
			}
			s.ingest(core.NormalizeOTLP(p))
			writeJSON(w, 200, map[string]any{}) // OTLP success: the collector expects an empty object
			return nil
		}
		var raw any
		if err := json.Unmarshal(body, &raw); err != nil {
			return err
		}
		events := core.CoerceEvents(raw)
		s.ingest(events)
		writeJSON(w, 200, map[string]int{"accepted": len(events)})
	case get && path == "/healthz":
		writeJSON(w, 200, map[string]string{"status": "ok"})
	case get && path == "/api/traces":
		writeJSON(w, 200, map[string]any{"traces": s.recentSummaries(finiteOr(jsNumber(q, "limit", "100"), 100))})
	case get && strings.HasPrefix(path, "/api/traces/"):
		id, err := url.PathUnescape(strings.TrimPrefix(path, "/api/traces/"))
		if err != nil {
			return err
		}
		// Fall back to history for traces that have aged out of the live window.
		if t := s.window.Assemble(id); t != nil {
			writeJSON(w, 200, core.Detail(t))
			return nil
		}
		raw, err := s.store.GetDetail(id)
		if err != nil {
			return err
		}
		if raw == nil {
			writeJSON(w, 404, map[string]string{"error": "trace_not_found"})
			return nil
		}
		w.Header().Set("content-type", "application/json; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write(raw)
	case get && path == "/api/topology":
		topology := s.window.Topology()
		writeJSON(w, 200, map[string]any{"topology": topology, "mermaidFlow": core.ToMermaidFlow(topology)})
	case get && strings.HasPrefix(path, "/api/service/"):
		name, err := url.PathUnescape(strings.TrimPrefix(path, "/api/service/"))
		if err != nil {
			return err
		}
		writeJSON(w, 200, s.serviceDetail(name))
	case get && path == "/api/errors":
		groups, err := s.store.ErrorGroups(int(positiveOr(jsNumber(q, "limit", "100"), 100)))
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"groups": groups})
	case get && path == "/api/search":
		return s.search(w, q)
	case get && path == "/api/days":
		days, err := s.store.Days()
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"days": days})
	case get && dayRoute.MatchString(path):
		m := dayRoute.FindStringSubmatch(path)
		day, err := url.PathUnescape(m[1])
		if err != nil {
			return err
		}
		return s.dayRoute(w, q, day, m[2])
	case get:
		s.serveStatic(w, r.URL.Path)
	default:
		writeJSON(w, 404, map[string]string{"error": "not_found"})
	}
	return nil
}

func (s *Server) dayRoute(w http.ResponseWriter, q url.Values, day, view string) error {
	endpoint := q.Get("endpoint")
	if (view == "latency" || view == "flamegraph") && endpoint == "" {
		writeJSON(w, 400, map[string]string{"error": "endpoint_required"})
		return nil
	}
	switch view {
	case "latency":
		buckets, err := s.store.EndpointLatency(day, endpoint)
		if err != nil {
			return err
		}
		dist, err := s.store.EndpointDistribution(day, endpoint)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"buckets": buckets, "distribution": dist})
	case "flamegraph":
		flame, err := s.store.EndpointFlamegraph(day, endpoint, 150)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"flame": flame})
	case "summary":
		sum, err := s.store.DaySummary(day)
		if err != nil {
			return err
		}
		writeJSON(w, 200, sum)
	case "traces":
		traces, err := s.store.DayTraces(day, int(finiteOr(jsNumber(q, "limit", "200"), 200)))
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"traces": traces})
	}
	return nil
}

func (s *Server) search(w http.ResponseWriter, q url.Values) error {
	posInt := func(name string) float64 {
		n := jsNumber(q, name, "")
		if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
			return 0
		}
		return n
	}
	o := history.SearchOptions{
		Q: q.Get("q"), Service: q.Get("service"), Attr: q.Get("attr"), TraceID: q.Get("traceId"),
		MinMicros: int64(posInt("minMs") * 1000), MaxMicros: int64(posInt("maxMs") * 1000),
		MinSpans: int(math.Ceil(posInt("minSpans"))), Limit: int(posInt("limit")), Sort: "recent",
	}
	if q.Get("sort") == "slowest" {
		o.Sort = "slowest"
	}
	if e := q.Get("error"); e != "" {
		v := e == "true"
		o.Error = &v
	}
	traces, err := s.store.Search(o)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"traces": traces})
	return nil
}

// jsNumber mirrors Number(params.get(name) ?? def): missing → def, blank → 0, junk → NaN.
func jsNumber(q url.Values, name, def string) float64 {
	raw := def
	if q.Has(name) {
		raw = q.Get(name)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return math.NaN()
	}
	return n
}

func finiteOr(n, def float64) float64 {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return def
	}
	return n
}

func positiveOr(n, def float64) float64 {
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return def
	}
	return n
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, errTooBig
		}
		return nil, err
	}
	if !strings.Contains(r.Header.Get("Content-Encoding"), "gzip") {
		return raw, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	// Cap decompression so a small gzip payload can't expand to gigabytes (zip bomb).
	out, err := io.ReadAll(io.LimitReader(zr, maxDecompressedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxDecompressedBytes {
		return nil, errors.New("decompressed body too large")
	}
	return out, nil
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := marshal(v)
	if err != nil {
		status, data = 500, []byte(`{"error":"internal"}`)
	}
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

var mimeTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
}

// serveStatic serves the built UI with an SPA fallback to index.html for unknown routes.
func (s *Server) serveStatic(w http.ResponseWriter, urlPath string) {
	if s.opts.PublicDir == "" {
		writeJSON(w, 404, map[string]string{"error": "not_found"})
		return
	}
	root, err := filepath.Abs(s.opts.PublicDir)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "not_found"})
		return
	}
	if urlPath == "/" {
		urlPath = "/index.html"
	}
	file := filepath.Join(root, filepath.FromSlash(filepath.Clean("/"+urlPath)))
	if !strings.HasPrefix(file, root+string(filepath.Separator)) {
		writeJSON(w, 403, map[string]string{"error": "forbidden"})
		return
	}
	data, err := os.ReadFile(file)
	if err != nil {
		file = filepath.Join(root, "index.html")
		if data, err = os.ReadFile(file); err != nil {
			writeJSON(w, 404, map[string]string{"error": "not_found"})
			return
		}
	}
	ct := mimeTypes[filepath.Ext(file)]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("content-type", ct)
	w.WriteHeader(200)
	_, _ = w.Write(data)
}
