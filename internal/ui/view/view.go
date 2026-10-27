// Package view is the UI's pure presentation logic: formatting, topology health, the dependency
// matrix and the flow-graph layout. Ports of ../live-probing/packages/ui/src/lib — templates are
// projections of these, and they are unit-tested without a browser.
package view

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"liveprobe/internal/core"
)

// --- format --------------------------------------------------------------------------------

var datastores = []string{"postgresql", "postgres", "redis", "rabbitmq", "mysql", "mongodb", "kafka", "memcached", "elasticsearch", "amqp"}

// IsDatastore matches datastore node ids, including compound names like "orders-postgresql".
func IsDatastore(nodeID string) bool {
	id := strings.ToLower(nodeID)
	for _, ds := range datastores {
		if strings.Contains(id, ds) {
			return true
		}
	}
	return false
}

func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

// FormatMicros formats microseconds as µs / ms / s, exactly like the TS formatMicros.
func FormatMicros(micros float64) string {
	if micros < 1000 {
		return core.FormatJSNumber(jsRound(micros)) + "µs"
	}
	ms := micros / 1000
	if ms < 1000 {
		if ms < 10 {
			return core.ToFixed(ms, 1) + "ms"
		}
		return core.FormatJSNumber(jsRound(ms)) + "ms"
	}
	s := ms / 1000
	if s < 10 {
		return core.ToFixed(s, 2) + "s"
	}
	return core.ToFixed(s, 1) + "s"
}

// Micros is FormatMicros for integer micros (the common case in templates).
func Micros(m int64) string { return FormatMicros(float64(m)) }

// HashString is FNV-1a over UTF-16 code units (JS charCodeAt), as the TS hashString.
func HashString(s string) uint32 {
	h := uint32(2166136261)
	for _, u := range utf16.Encode([]rune(s)) {
		h ^= uint32(u)
		h *= 16777619
	}
	return h
}

// Pct formats a 0..1 rate as a percentage: 1 decimal under 10%, none from 10% up.
func Pct(rate float64) string {
	digits := 1
	if rate >= 0.1 {
		digits = 0
	}
	return core.ToFixed(rate*100, digits) + "%"
}

// Rate formats a per-second rate for the RED tiles.
func Rate(r float64) string {
	switch {
	case r >= 100:
		return core.FormatJSNumber(jsRound(r)) + "/s"
	case r >= 10:
		return core.ToFixed(r, 0) + "/s"
	}
	return core.ToFixed(r, 1) + "/s"
}

// Num formats a number the way JS template literals do.
func Num(x float64) string { return core.FormatJSNumber(x) }

// Itoa is strconv.Itoa, exported for templates.
func Itoa(n int) string { return strconv.Itoa(n) }

// --- topology health -----------------------------------------------------------------------

// Health is the traffic-light level of a node or edge in the live flow graph.
type Health string

const (
	OK    Health = "ok"
	Warn  Health = "warn"
	Error Health = "error"
)

type Thresholds struct {
	ErrorRateError    float64
	ErrorRateWarn     float64
	LatencyWarnFactor float64 // amber when avg latency >= factor × median edge latency
}

// DefaultThresholds match core.DefaultAlertThresholds, so the graph, RED tiles and alerts agree.
var DefaultThresholds = Thresholds{ErrorRateError: 0.05, ErrorRateWarn: 0.01, LatencyWarnFactor: 2}

type HealthResult struct {
	EdgeHealth          []Health          // parallel to topology.Edges
	NodeHealth          map[string]Health // by the calls *into* each node (points at the culprit)
	MedianLatencyMicros float64
}

func worst(a, b Health) Health {
	rank := map[Health]int{OK: 0, Warn: 1, Error: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := slices.Clone(values)
	slices.Sort(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

func errorRateHealth(errors, calls int, t Thresholds) Health {
	if calls <= 0 {
		return OK
	}
	rate := float64(errors) / float64(calls)
	switch {
	case rate >= t.ErrorRateError:
		return Error
	case rate >= t.ErrorRateWarn:
		return Warn
	}
	return OK
}

// ServiceHealth classifies a RED error rate with the same bands.
func ServiceHealth(errorRate float64) Health {
	switch {
	case errorRate >= DefaultThresholds.ErrorRateError:
		return Error
	case errorRate >= DefaultThresholds.ErrorRateWarn:
		return Warn
	}
	return OK
}

// TopologyHealth classifies every edge and node into ok / warn / error from the edge counts the
// graph already carries. Latency only ever raises to amber; errors decide red.
func TopologyHealth(topo core.Topology, t Thresholds) HealthResult {
	var durs []float64
	for _, e := range topo.Edges {
		if e.Calls > 0 {
			durs = append(durs, float64(e.AvgDurationMicros))
		}
	}
	med := median(durs)
	slowCutoff := med * t.LatencyWarnFactor

	res := HealthResult{EdgeHealth: make([]Health, len(topo.Edges)), NodeHealth: map[string]Health{}, MedianLatencyMicros: med}
	for i, e := range topo.Edges {
		byError := errorRateHealth(e.Errors, e.Calls, t)
		if byError == Error {
			res.EdgeHealth[i] = Error
			continue
		}
		slow := med > 0 && e.Calls > 0 && float64(e.AvgDurationMicros) >= slowCutoff
		res.EdgeHealth[i] = byError
		if slow {
			res.EdgeHealth[i] = worst(byError, Warn)
		}
	}

	for _, n := range topo.Nodes {
		res.NodeHealth[n] = OK
	}
	type acc struct {
		errors, calls int
		slow          bool
	}
	inbound := map[string]*acc{}
	var order []string
	for i, e := range topo.Edges {
		a := inbound[e.To]
		if a == nil {
			a = &acc{}
			inbound[e.To] = a
			order = append(order, e.To)
		}
		a.errors += e.Errors
		a.calls += e.Calls
		if res.EdgeHealth[i] == Warn && e.Errors == 0 {
			a.slow = true // latency-driven warn
		}
	}
	for _, node := range order {
		a := inbound[node]
		h := errorRateHealth(a.errors, a.calls, t)
		if a.slow {
			h = worst(h, Warn)
		}
		res.NodeHealth[node] = h
	}
	return res
}

// --- dependency matrix ---------------------------------------------------------------------

type MatrixCell struct {
	From, To          string
	Calls, Errors     int
	ErrorRate         float64
	AvgDurationMicros int64
	Health            Health
}

type Matrix struct {
	Rows, Cols          []string // callers and callees, sorted
	Cells               []MatrixCell
	byKey               map[[2]string]int
	MedianLatencyMicros float64
}

func (m Matrix) Cell(from, to string) (MatrixCell, bool) {
	i, ok := m.byKey[[2]string{from, to}]
	if !ok {
		return MatrixCell{}, false
	}
	return m.Cells[i], true
}

// MaxCalls is the busiest cell's call count (>= 1), for opacity scaling.
func (m Matrix) MaxCalls() int {
	mx := 1
	for _, c := range m.Cells {
		mx = max(mx, c.Calls)
	}
	return mx
}

// DependencyMatrix projects a topology into a caller×callee adjacency matrix; cells exist only
// where a call was observed and reuse TopologyHealth's edge levels.
func DependencyMatrix(topo core.Topology, t Thresholds) Matrix {
	h := TopologyHealth(topo, t)
	m := Matrix{Rows: []string{}, Cols: []string{}, Cells: []MatrixCell{}, byKey: map[[2]string]int{}, MedianLatencyMicros: h.MedianLatencyMicros}
	rows, cols := map[string]bool{}, map[string]bool{}
	for i, e := range topo.Edges {
		if e.From == e.To {
			continue // a self-loop isn't a dependency
		}
		rows[e.From], cols[e.To] = true, true
		rate := 0.0
		if e.Calls > 0 {
			rate = float64(e.Errors) / float64(e.Calls)
		}
		m.byKey[[2]string{e.From, e.To}] = len(m.Cells)
		m.Cells = append(m.Cells, MatrixCell{From: e.From, To: e.To, Calls: e.Calls, Errors: e.Errors, ErrorRate: rate, AvgDurationMicros: e.AvgDurationMicros, Health: h.EdgeHealth[i]})
	}
	for r := range rows {
		m.Rows = append(m.Rows, r)
	}
	for c := range cols {
		m.Cols = append(m.Cols, c)
	}
	slices.Sort(m.Rows) // JS default sort compares UTF-16 code units; identical for BMP ids
	slices.Sort(m.Cols)
	return m
}

// --- flow layout ---------------------------------------------------------------------------

type Point struct{ X, Y float64 }

type Layout struct {
	Positions     map[string]Point
	Width, Height float64
}

// ComputeLayout is a deterministic layered layout with barycenter ordering: sources left,
// datastores right, the rest by BFS depth; within a column, nodes are ordered by the mean row of
// their neighbours over a few sweeps. Seeded by a hash of the node id so it is stable across
// telemetry deltas (depends only on the node set + edge structure).
func ComputeLayout(nodes []string, edges []core.Edge) Layout {
	const colW, rowH, marginX, marginY = 190.0, 76.0, 80.0, 56.0

	nodeSet := map[string]bool{}
	inbound := map[string]int{}
	outAdj, inAdj := map[string][]string{}, map[string][]string{}
	for _, n := range nodes {
		nodeSet[n] = true
	}
	for _, e := range edges {
		if !nodeSet[e.From] || !nodeSet[e.To] {
			continue
		}
		inbound[e.To]++
		outAdj[e.From] = append(outAdj[e.From], e.To)
		inAdj[e.To] = append(inAdj[e.To], e.From)
	}

	// BFS depth from source nodes assigns a base column.
	depth := map[string]int{}
	var queue []string
	for _, n := range nodes {
		if inbound[n] == 0 {
			depth[n] = 0
			queue = append(queue, n)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		d := depth[cur]
		for _, next := range outAdj[cur] {
			if nd, seen := depth[next]; !seen || nd < d+1 {
				if !seen {
					queue = append(queue, next)
				}
				depth[next] = d + 1
			}
		}
	}
	maxDepth := 0
	for _, n := range nodes {
		if _, ok := depth[n]; !ok {
			depth[n] = 1
		}
		maxDepth = max(maxDepth, depth[n])
	}

	byCol := map[int][]string{}
	for _, n := range nodes {
		c := depth[n]
		if IsDatastore(n) {
			c = maxDepth + 1 // datastores forced to the far-right column
		}
		byCol[c] = append(byCol[c], n)
	}
	var cols []int
	for c := range byCol {
		cols = append(cols, c)
	}
	slices.Sort(cols)

	order := map[int][]string{}
	for _, c := range cols {
		o := slices.Clone(byCol[c])
		slices.SortStableFunc(o, func(a, b string) int { return cmp.Compare(HashString(a), HashString(b)) })
		order[c] = o
	}
	reorder := func(c, neighborCol int, adj map[string][]string) {
		idx := map[string]int{}
		for i, n := range order[neighborCol] {
			idx[n] = i
		}
		type keyed struct {
			n string
			i int
			b float64
		}
		ks := make([]keyed, len(order[c]))
		for i, n := range order[c] {
			sum, cnt := 0.0, 0
			for _, x := range adj[n] {
				if v, ok := idx[x]; ok {
					sum += float64(v)
					cnt++
				}
			}
			b := float64(i)
			if cnt > 0 {
				b = sum / float64(cnt)
			}
			ks[i] = keyed{n, i, b}
		}
		slices.SortStableFunc(ks, func(p, q keyed) int { return cmp.Or(cmp.Compare(p.b, q.b), cmp.Compare(p.i, q.i)) })
		for i, k := range ks {
			order[c][i] = k.n
		}
	}
	for range 4 {
		for ci := 1; ci < len(cols); ci++ {
			reorder(cols[ci], cols[ci-1], inAdj) // down
		}
		for ci := len(cols) - 2; ci >= 0; ci-- {
			reorder(cols[ci], cols[ci+1], outAdj) // up
		}
	}

	l := Layout{Positions: map[string]Point{}}
	maxRows := 1
	for _, c := range cols {
		maxRows = max(maxRows, len(order[c]))
	}
	// Vertically center each column against the tallest, so rows line up mid-graph.
	fullHeight := float64(maxRows) * rowH
	for _, c := range cols {
		offset := (fullHeight - float64(len(order[c]))*rowH) / 2
		for i, n := range order[c] {
			l.Positions[n] = Point{marginX + float64(c)*colW, marginY + float64(i)*rowH + offset}
		}
	}
	maxCol := 0
	if len(cols) > 0 {
		maxCol = cols[len(cols)-1]
	}
	l.Width = marginX*2 + float64(maxCol)*colW
	l.Height = marginY*2 + float64(max(0, maxRows-1))*rowH
	return l
}
