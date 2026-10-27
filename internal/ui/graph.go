package ui

import (
	"math"
	"slices"
	"strings"
	"sync"
	"unicode/utf16"

	"liveprobe/internal/core"
	"liveprobe/internal/ui/view"
)

// Geometry for the flow graph and dependency matrix. Pure functions of the topology; the templ
// templates only print these. Ported from ui/src/components/{flow-view,dependency-matrix}.tsx.

const (
	nodeW, nodeH, fitPad = 132.0, 44.0, 70.0

	cellSize   = 30.0
	rowLabelW  = 132.0
	colLabelH  = 96.0
	matrixPad  = 8.0
	labelMax   = 18
	nodeLabelM = 16
)

// truncate cuts to max UTF-16 units with an ellipsis, as the TS s.slice(0, n) does.
func truncate(s string, maxUnits int) string {
	u := utf16.Encode([]rune(s))
	if len(u) <= maxUnits {
		return s
	}
	return string(utf16.Decode(u[:maxUnits-1])) + "…"
}

// filterTopology keeps a service and its direct neighbours (only the edges touching it).
func filterTopology(t core.Topology, service string) core.Topology {
	if service == "" {
		return t
	}
	out := core.Topology{Nodes: []string{service}, Externals: t.Externals}
	for _, e := range t.Edges {
		if e.From != service && e.To != service {
			continue
		}
		out.Edges = append(out.Edges, e)
		for _, n := range []string{e.From, e.To} {
			if !slices.Contains(out.Nodes, n) {
				out.Nodes = append(out.Nodes, n)
			}
		}
	}
	return out
}

// Layout is recomputed only when the *set* of node ids changes, so call-count deltas never
// reflow the graph. One cache serves every client (the layout is deterministic).
var layoutCache = struct {
	sync.Mutex
	m map[string]view.Layout
}{m: map[string]view.Layout{}}

func cachedLayout(t core.Topology) view.Layout {
	ids := slices.Clone(t.Nodes)
	slices.Sort(ids)
	key := strings.Join(ids, "\x00")
	layoutCache.Lock()
	defer layoutCache.Unlock()
	if l, ok := layoutCache.m[key]; ok {
		return l
	}
	if len(layoutCache.m) > 256 {
		clear(layoutCache.m)
	}
	l := view.ComputeLayout(t.Nodes, t.Edges)
	layoutCache.m[key] = l
	return l
}

type flowNode struct {
	Label, Class, LabelClass, Href string
	Left, Top, X, LabelY, Rx       string
}

type flowEdge struct {
	Path, Class, Marker, Width string
	LX, LY, SubY, Label, Sub   string
}

type flowGraph struct {
	Empty bool
	Nodes []flowNode
	Edges []flowEdge
	Fit   string // viewBox "x y w h" enclosing every node
}

func num(x float64) string { return view.Num(x) }

// anchor is where a line from a-centre to b-centre exits a's (padded) box.
func anchor(from, to view.Point, halfW, halfH float64) view.Point {
	dx, dy := to.X-from.X, to.Y-from.Y
	if dx == 0 && dy == 0 {
		return from
	}
	sx, sy := math.Inf(1), math.Inf(1)
	if dx != 0 {
		sx = halfW / math.Abs(dx)
	}
	if dy != 0 {
		sy = halfH / math.Abs(dy)
	}
	s := math.Min(sx, sy)
	return view.Point{X: from.X + dx*s, Y: from.Y + dy*s}
}

func buildFlow(full core.Topology, service string) flowGraph {
	t := filterTopology(full, service)
	if len(t.Nodes) == 0 {
		return flowGraph{Empty: true, Fit: "0 0 400 300"}
	}
	l := cachedLayout(t)
	health := view.TopologyHealth(t, view.DefaultThresholds)
	externals := map[string]bool{}
	for _, x := range t.Externals {
		externals[x] = true
	}
	g := flowGraph{}
	halfW, halfH := nodeW/2+6, nodeH/2+6

	for i, e := range t.Edges {
		a, okA := l.Positions[e.From]
		b, okB := l.Positions[e.To]
		if !okA || !okB || e.From == e.To {
			continue
		}
		start, end := anchor(a, b, halfW, halfH), anchor(b, a, halfW, halfH)
		mx, my := (start.X+end.X)/2, (start.Y+end.Y)/2
		dx, dy := end.X-start.X, end.Y-start.Y
		length := math.Hypot(dx, dy)
		if length == 0 {
			length = 1
		}
		bend := math.Min(28, length*0.12)
		cx, cy := mx+(-dy/length)*bend, my+(dx/length)*bend
		h := health.EdgeHealth[i]
		fe := flowEdge{
			Path:   "M " + num(start.X) + " " + num(start.Y) + " Q " + num(cx) + " " + num(cy) + " " + num(end.X) + " " + num(end.Y),
			Class:  "flow-edge",
			Marker: "url(#arrow)",
			Width:  num(1 + math.Log(1+math.Max(0, float64(e.Calls)))*1.4),
			LX:     num(cx), LY: num(cy - 4), SubY: num(cy + 9),
			Label: view.Itoa(e.Calls),
			Sub:   view.Micros(e.AvgDurationMicros),
		}
		if e.Errors > 0 {
			fe.Label += " · " + view.Itoa(e.Errors) + " err"
		}
		if h != view.OK {
			fe.Class += " flow-edge-" + string(h)
			fe.Marker = "url(#arrow-" + string(h) + ")"
		}
		g.Edges = append(g.Edges, fe)
	}

	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, n := range t.Nodes {
		p, ok := l.Positions[n]
		if !ok {
			continue
		}
		minX, minY, maxX, maxY = math.Min(minX, p.X), math.Min(minY, p.Y), math.Max(maxX, p.X), math.Max(maxY, p.Y)
		ds, ghost := view.IsDatastore(n), externals[n]
		class := "flow-node flow-node-service"
		switch {
		case ds:
			class = "flow-node flow-node-datastore"
		case ghost:
			class = "flow-node flow-node-external"
		}
		if h := health.NodeHealth[n]; h != "" && h != view.OK {
			class += " flow-node-" + string(h) // health wins in the cascade
		}
		rx := 7.0
		if ds {
			rx = nodeH / 2
		}
		fn := flowNode{
			Label: truncate(n, nodeLabelM), Class: class, LabelClass: "flow-node-label",
			Left: num(p.X - nodeW/2), Top: num(p.Y - nodeH/2), X: num(p.X), LabelY: num(p.Y + 4), Rx: num(rx),
		}
		if ghost {
			fn.LabelClass += " flow-node-label-external"
		}
		if !ds && !ghost {
			fn.Href = serviceHref(n) // datastores and ghosts have no service page
		}
		g.Nodes = append(g.Nodes, fn)
	}
	if math.IsInf(minX, 1) {
		g.Fit = "0 0 400 300"
	} else {
		g.Fit = num(minX-nodeW/2-fitPad) + " " + num(minY-nodeH/2-fitPad) + " " + num(maxX-minX+nodeW+fitPad*2) + " " + num(maxY-minY+nodeH+fitPad*2)
	}
	return g
}

// --- dependency matrix ---------------------------------------------------------------------

type matrixLabel struct {
	Text, Class, Href, X, Y, Transform string
}

type matrixCell struct {
	Empty                   bool
	X, Y, Size, Fill, Style string
	Text, TextX, TextY      string
	Title, Class, Href      string
}

type matrixGraph struct {
	Empty      bool
	W, H       string
	ColLabels  []matrixLabel
	RowLabels  []matrixLabel
	Cells      []matrixCell
	MetricLink map[string]string // metric → href, for the toggle
}

var healthFill = map[view.Health]string{view.OK: "var(--live)", view.Warn: "var(--new)", view.Error: "var(--error)"}

func cellText(c view.MatrixCell, metric string) string {
	switch metric {
	case "errors":
		if c.Errors > 0 {
			return view.Itoa(c.Errors)
		}
		return ""
	case "latency":
		return view.Micros(c.AvgDurationMicros)
	}
	if c.Calls >= 1000 { // compact so a busy cell doesn't overflow
		digits := 0
		if c.Calls < 10000 {
			digits = 1
		}
		return core.ToFixed(float64(c.Calls)/1000, digits) + "k"
	}
	return view.Itoa(c.Calls)
}

func buildMatrix(full core.Topology, p liveParams) matrixGraph {
	t := filterTopology(full, p.Service)
	m := view.DependencyMatrix(t, view.DefaultThresholds)
	mg := matrixGraph{MetricLink: map[string]string{}}
	for _, metric := range []string{"calls", "errors", "latency"} {
		mg.MetricLink[metric] = p.href(func(c *liveParams) { c.Metric = metric })
	}
	if len(m.Rows) == 0 || len(m.Cols) == 0 {
		mg.Empty = true
		return mg
	}
	externals := map[string]bool{}
	for _, x := range t.Externals {
		externals[x] = true
	}
	link := func(id string) string {
		if view.IsDatastore(id) || externals[id] {
			return ""
		}
		return serviceHref(id)
	}
	labelClass := func(base, href string) string {
		if href != "" {
			return base + " clickable"
		}
		return base
	}
	mg.W = num(rowLabelW + float64(len(m.Cols))*cellSize + matrixPad)
	mg.H = num(colLabelH + float64(len(m.Rows))*cellSize + matrixPad)
	for ci, c := range m.Cols {
		x, y := rowLabelW+float64(ci)*cellSize+cellSize/2, colLabelH-6
		href := link(c)
		mg.ColLabels = append(mg.ColLabels, matrixLabel{Text: truncate(c, labelMax), Class: labelClass("depmatrix-collabel", href), Href: href,
			X: num(x), Y: num(y), Transform: "rotate(-55 " + num(x) + " " + num(y) + ")"})
	}
	for ri, r := range m.Rows {
		href := link(r)
		mg.RowLabels = append(mg.RowLabels, matrixLabel{Text: truncate(r, labelMax), Class: labelClass("depmatrix-rowlabel", href), Href: href,
			X: num(rowLabelW - 8), Y: num(colLabelH + float64(ri)*cellSize + cellSize/2 + 4)})
	}
	maxCalls := float64(m.MaxCalls())
	for ri, r := range m.Rows {
		for ci, c := range m.Cols {
			x, y := rowLabelW+float64(ci)*cellSize, colLabelH+float64(ri)*cellSize
			cell, ok := m.Cell(r, c)
			mc := matrixCell{X: num(x + 1), Y: num(y + 1), Size: num(cellSize - 2)}
			if !ok {
				mc.Empty = true // a faint slot so the grid structure reads
				mg.Cells = append(mg.Cells, mc)
				continue
			}
			opacity := 0.28 + 0.62*(math.Log(1+float64(cell.Calls))/math.Log(1+maxCalls))
			mc.Style = "fill:" + healthFill[cell.Health] + ";opacity:" + num(opacity)
			mc.Text = cellText(cell, p.Metric)
			mc.TextX, mc.TextY = num(x+cellSize/2), num(y+cellSize/2+3)
			mc.Href = link(c)
			mc.Class = labelClass("depmatrix-cell", mc.Href)
			mc.Title = r + " → " + c + ": " + view.Itoa(cell.Calls) + " calls"
			if cell.Errors > 0 {
				mc.Title += ", " + view.Itoa(cell.Errors) + " err (" + core.ToFixed(cell.ErrorRate*100, 1) + "%)"
			}
			mc.Title += ", avg " + view.Micros(cell.AvgDurationMicros)
			mg.Cells = append(mg.Cells, mc)
		}
	}
	return mg
}

func utf16Units(s string) []uint16 { return utf16.Encode([]rune(s)) }
