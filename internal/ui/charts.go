package ui

import (
	"math"
	"strconv"
	"strings"
	"time"

	"liveprobe/internal/core"
	"liveprobe/internal/history"
	"liveprobe/internal/ui/view"
)

// Chart geometry for the history/day pages. Ports of ui/src/components/{trends,throughput,latency}-chart,
// latency-{histogram,heatmap} and flamegraph-view; templates in pages.templ only print these.

// grouped formats an integer with en-US thousands separators (React used toLocaleString(), which
// follows the *viewer's* locale; a server can't know it, so en-US is the documented choice).
func grouped(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// hhmm is a UTC "HH:MM" for an epoch-minutes value.
func hhmm(minute int64) string { return time.UnixMilli(minute * 60000).UTC().Format("15:04") }

// --- trends (history page) -----------------------------------------------------------------

type trendBar struct {
	Title, X, Y, W, H, ErrY, ErrH, LabelX, Label string
	HasErr                                       bool
}

type trendsChart struct {
	MaxLabel, MaxY, BaseY string
	Bars                  []trendBar
}

func buildTrends(days []history.DayInfo) trendsChart {
	const w, h, padL, padR, padT, padB = 820.0, 200.0, 56.0, 14.0, 14.0, 28.0
	ordered := make([]history.DayInfo, len(days)) // oldest → newest, left → right
	for i, d := range days {
		ordered[len(days)-1-i] = d
	}
	maxReq := 1
	for _, d := range ordered {
		maxReq = max(maxReq, d.Requests)
	}
	slot := (w - padL - padR) / float64(len(ordered))
	barW := math.Min(48, slot*0.6)
	y := func(v float64) float64 { return padT + (1-v/float64(maxReq))*(h-padT-padB) }
	base := y(0)
	c := trendsChart{MaxLabel: grouped(maxReq), MaxY: num(y(float64(maxReq)) + 4), BaseY: num(base)}
	for i, d := range ordered {
		cx := padL + float64(i)*slot + slot/2
		top := y(float64(d.Requests))
		frac := 0.0
		if d.Requests > 0 {
			frac = float64(d.Errors) / float64(d.Requests)
		}
		errH := (base - top) * frac
		label := d.Day
		if len(label) > 5 {
			label = label[5:]
		}
		c.Bars = append(c.Bars, trendBar{
			Title: d.Day + ": " + grouped(d.Requests) + " requests, " + grouped(d.Errors) + " errors",
			X:     num(cx - barW/2), Y: num(top), W: num(barW), H: num(math.Max(0, base-top)),
			HasErr: errH > 0, ErrY: num(base - errH), ErrH: num(errH),
			LabelX: num(cx), Label: label,
		})
	}
	return c
}

// --- throughput (day page) -----------------------------------------------------------------

type tpBar struct {
	X, Y, W, H, Title, ErrY, ErrH string
	HasErr                        bool
}

type tpTick struct{ X, Label string }

type throughputChart struct {
	Empty         bool
	MaxCount      string
	MaxY, BaseY   string
	BaselineRight string
	Bars          []tpBar
	Ticks         []tpTick
}

func buildThroughput(buckets []history.ThroughputBucket) throughputChart {
	const w, h, padL, padR, padT, padB = 720.0, 180.0, 40.0, 12.0, 12.0, 26.0
	if len(buckets) == 0 {
		return throughputChart{Empty: true}
	}
	plotW, plotH := w-padL-padR, h-padT-padB
	maxCount := 1
	for _, b := range buckets {
		maxCount = max(maxCount, b.Count)
	}
	slot := plotW / float64(len(buckets))
	barW := math.Max(1, math.Min(slot*0.8, 48))
	baseline := padT + plotH
	c := throughputChart{MaxCount: view.Itoa(maxCount), MaxY: num(padT + 4), BaseY: num(baseline), BaselineRight: num(w - padR)}
	for i, b := range buckets {
		bh := float64(b.Count) / float64(maxCount) * plotH
		errH := 0.0
		if b.Count > 0 {
			errH = float64(min(b.Errors, b.Count)) / float64(maxCount) * plotH
		}
		c.Bars = append(c.Bars, tpBar{
			X: num(padL + slot*float64(i) + (slot-barW)/2), Y: num(padT + plotH - bh), W: num(barW), H: num(math.Max(0, bh)),
			Title:  hhmm(b.Minute) + " — " + view.Itoa(b.Count) + " req, " + view.Itoa(b.Errors) + " err",
			HasErr: errH > 0, ErrY: num(baseline - errH), ErrH: num(errH),
		})
	}
	tickCount := min(len(buckets), 6)
	seen := map[int]bool{}
	for t := range tickCount {
		i := 0
		if tickCount > 1 {
			i = int(math.Floor(float64(t*(len(buckets)-1))/float64(tickCount-1) + 0.5))
		}
		if !seen[i] {
			seen[i] = true
			c.Ticks = append(c.Ticks, tpTick{X: num(padL + slot*float64(i) + slot/2), Label: hhmm(buckets[i].Minute)})
		}
	}
	return c
}

// --- latency over time ---------------------------------------------------------------------

type latencyChart struct {
	Empty                       bool
	MaxLabel, MaxY, ZeroY       string
	FirstX, LastX, First, Last  string
	P50, P95, P99               string
	AxisLeft, AxisBottom, Right string
}

func buildLatency(buckets []history.LatencyBucket) latencyChart {
	const w, h, padL, padR, padT, padB = 820.0, 220.0, 52.0, 14.0, 14.0, 28.0
	if len(buckets) == 0 {
		return latencyChart{Empty: true}
	}
	maxY := 1.0
	for _, b := range buckets {
		maxY = math.Max(maxY, float64(b.P99))
	}
	first, last := buckets[0].Minute, buckets[len(buckets)-1].Minute
	span := math.Max(1, float64(last-first))
	x := func(m int64) float64 { return padL + float64(m-first)/span*(w-padL-padR) }
	y := func(v float64) float64 { return padT + (1-v/maxY)*(h-padT-padB) }
	path := func(pick func(history.LatencyBucket) int64) string {
		var b strings.Builder
		for i, k := range buckets {
			if i > 0 {
				b.WriteByte(' ')
			}
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			b.WriteString(cmd + " " + core.ToFixed(x(k.Minute), 1) + " " + core.ToFixed(y(float64(pick(k))), 1))
		}
		return b.String()
	}
	return latencyChart{
		MaxLabel: view.FormatMicros(maxY), MaxY: num(y(maxY) + 4), ZeroY: num(y(0)),
		FirstX: num(x(first)), LastX: num(x(last)), First: hhmm(first), Last: hhmm(last),
		P50:      path(func(b history.LatencyBucket) int64 { return b.P50 }),
		P95:      path(func(b history.LatencyBucket) int64 { return b.P95 }),
		P99:      path(func(b history.LatencyBucket) int64 { return b.P99 }),
		AxisLeft: num(padL), AxisBottom: num(h - padB), Right: num(w - padR),
	}
}

// --- latency histogram ---------------------------------------------------------------------

type histBar struct {
	X, Y, W, H, Title string
	ShowCount         bool
	Count, CountX     string
	CountY            string
	LabelX, LabelY    string
	Label, Transform  string
}

type histogramChart struct {
	Empty                    bool
	MaxLabel, MaxY, ZeroY    string
	Bottom, Left, Right, Top string
	Bars                     []histBar
}

func buildHistogram(hist []int) histogramChart {
	const w, h, padL, padR, padT, padB = 820.0, 200.0, 44.0, 14.0, 12.0, 52.0
	total, maxY := 0, 1
	for _, c := range hist {
		total += c
		maxY = max(maxY, c)
	}
	if total == 0 {
		return histogramChart{Empty: true}
	}
	plotW, plotH := w-padL-padR, h-padT-padB
	bw := plotW / float64(len(hist))
	y := func(v float64) float64 { return padT + (1-v/float64(maxY))*plotH }
	c := histogramChart{MaxLabel: view.Itoa(maxY), MaxY: num(y(float64(maxY)) + 4), ZeroY: num(y(0)),
		Bottom: num(h - padB), Left: num(padL), Right: num(w - padR), Top: num(padT)}
	for i, cnt := range hist {
		bx := padL + float64(i)*bw
		bh := 0.0
		if cnt > 0 {
			bh = math.Max(1, plotH*(float64(cnt)/float64(maxY)))
		}
		label := core.LatencyBucketLabels[i]
		pctOf := math.Floor(float64(cnt)/float64(total)*100 + 0.5)
		lx, ly := bx+bw/2, h-padB+12
		c.Bars = append(c.Bars, histBar{
			X: num(bx + 2), Y: num(h - padB - bh), W: num(math.Max(1, bw-4)), H: num(bh),
			Title:     label + ": " + view.Itoa(cnt) + " (" + view.Num(pctOf) + "%)",
			ShowCount: cnt > 0 && bw > 26, Count: view.Itoa(cnt), CountX: num(bx + bw/2), CountY: num(h - padB - bh - 3),
			LabelX: num(lx), LabelY: num(ly), Label: label, Transform: "rotate(-40 " + num(lx) + " " + num(ly) + ")",
		})
	}
	return c
}

// --- latency heatmap -----------------------------------------------------------------------

type heatCell struct{ X, Y, W, H, Style, Title string }

type heatLabel struct{ Y, Label string }

type heatmapChart struct {
	Empty                      bool
	H                          string
	YLabels                    []heatLabel
	Cells                      []heatCell
	FirstX, LastX, LabelY      string
	First, Last, LabelRightPad string
}

func buildHeatmap(rows []history.HeatmapRow) heatmapChart {
	const w, padL, padR, padT, padB, rowH = 820.0, 66.0, 14.0, 10.0, 26.0, 15.0
	if len(rows) == 0 {
		return heatmapChart{Empty: true}
	}
	n := len(core.LatencyBucketLabels)
	h := padT + padB + float64(n)*rowH
	plotW := w - padL - padR
	cw := plotW / float64(len(rows))
	maxCount := 1
	for _, r := range rows {
		for _, c := range r.Counts {
			maxCount = max(maxCount, c)
		}
	}
	rowY := func(b int) float64 { return padT + float64(n-1-b)*rowH } // slowest on top
	c := heatmapChart{H: num(h), FirstX: num(padL), LastX: num(padL + plotW), LabelY: num(h - padB + 18),
		First: hhmm(rows[0].Minute), Last: hhmm(rows[len(rows)-1].Minute)}
	for b, label := range core.LatencyBucketLabels {
		if b%2 == 0 {
			c.YLabels = append(c.YLabels, heatLabel{Y: num(rowY(b) + rowH - 4), Label: label})
		}
	}
	for ci, col := range rows {
		for b, cnt := range col.Counts {
			if cnt == 0 {
				continue
			}
			c.Cells = append(c.Cells, heatCell{
				X: num(padL + float64(ci)*cw), Y: num(rowY(b)), W: num(math.Max(1, cw-0.5)), H: num(rowH - 0.5),
				Style: "opacity:" + num(0.12+0.88*(float64(cnt)/float64(maxCount))),
				Title: hhmm(col.Minute) + " · " + core.LatencyBucketLabels[b] + ": " + view.Itoa(cnt),
			})
		}
	}
	return c
}

// --- flamegraph ----------------------------------------------------------------------------

type flameCell struct {
	Class, X, Y, W, Title, Hover string
	ShowLabel                    bool
	LabelX, LabelY, Label        string
}

type flamegraph struct {
	Empty bool
	H     string
	Cells []flameCell
}

// flameColor mirrors the TS colorClass: a 32-bit wrapping h*31+c hash over UTF-16 units, 6 buckets.
func flameColor(participant string) string {
	var h int32
	for _, u := range utf16Units(participant) {
		h = h*31 + int32(u)
	}
	a := int64(h)
	if a < 0 {
		a = -a
	}
	return "flame-c" + strconv.FormatInt(a%6, 10)
}

func buildFlamegraph(root *core.FlameNode) flamegraph {
	const w, row, minW = 820.0, 22.0, 0.5
	if root == nil || len(root.Children) == 0 {
		return flamegraph{Empty: true}
	}
	var grand int64
	for _, c := range root.Children {
		grand += c.TotalMicros
	}
	if grand == 0 {
		grand = 1
	}
	pct := func(n *core.FlameNode) string {
		return view.Num(math.Floor(float64(n.TotalMicros)/float64(grand)*100 + 0.5))
	}
	fg := flamegraph{}
	var layout func(n *core.FlameNode, x, width float64, depth int) int
	layout = func(n *core.FlameNode, x, width float64, depth int) int {
		if width < minW {
			return depth
		}
		summary := n.Participant + " · " + n.Operation
		stats := "total " + view.Micros(n.TotalMicros) + " · self " + view.Micros(n.SelfMicros) + " · " + view.Itoa(n.Count) + " calls (" + pct(n) + "%)"
		fg.Cells = append(fg.Cells, flameCell{
			Class: flameColor(n.Participant), X: num(x + 0.5), Y: num(float64(depth) * row), W: num(math.Max(0.5, width-1)),
			Title:     summary + "\n" + stats,
			Hover:     summary + " — total " + view.Micros(n.TotalMicros) + ", self " + view.Micros(n.SelfMicros) + ", " + view.Itoa(n.Count) + " calls (" + pct(n) + "%)",
			ShowLabel: width > 44, LabelX: num(x + 5), LabelY: num(float64(depth)*row + row/2 + 3), Label: n.Operation,
		})
		total := float64(n.TotalMicros)
		if total == 0 {
			total = 1
		}
		cx, maxDepth := x, depth
		for _, c := range n.Children {
			cw := width * (float64(c.TotalMicros) / total)
			maxDepth = max(maxDepth, layout(c, cx, cw, depth+1))
			cx += cw
		}
		return maxDepth
	}
	cx, maxDepth := 0.0, 0
	for _, c := range root.Children {
		cw := w * (float64(c.TotalMicros) / float64(grand))
		maxDepth = max(maxDepth, layout(c, cx, cw, 0))
		cx += cw
	}
	fg.H = num(float64(maxDepth+1) * row)
	return fg
}
