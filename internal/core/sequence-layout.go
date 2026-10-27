package core

import (
	"cmp"
	"slices"
	"strings"
)

// LayoutSequence turns a flat, time-ordered message list into a UML-style call/return layout:
// each synchronous call gets an activation bar on the callee's lifeline (from the call arrow
// down to its return arrow), and rows are bracketed by nesting so a parent's return lands after
// all of its children's. Pure — the renderer is a projection of this.
//
// Nesting is inferred from *time containment* (a synchronous parent span is active while its
// children run, so it contains them), not from parent ids. Async messages (producer/consumer)
// are fire-and-forget: a call row, no return, no activation.

type LaidOutMessage struct {
	Message
	CallRow   int            `json:"callRow"`             // row index of the call arrow
	ReturnRow int            `json:"returnRow"`           // row of the return arrow (== CallRow for async)
	Depth     int            `json:"depth"`               // activations already open on the callee lifeline (x-offset lane)
	Group     *SequenceGroup `json:"group,omitempty"`     // set on a collapsed synthetic row and on an expanded group's lead row
	GroupRole string         `json:"groupRole,omitempty"` // "collapsed" | "lead" | "member"
}

// SequenceGroup is a run of repeated sibling *leaf* calls with the same signature — the shape an
// N+1 query (or any hot loop) makes. Collapsing it into one ×N row keeps the diagram readable
// AND names the problem, so it carries the count and aggregate timing rather than hiding them.
type SequenceGroup struct {
	ID                  string   `json:"id"` // the earliest member's spanId; also the synthetic row's spanId
	Signature           string   `json:"signature"`
	From                string   `json:"from"`
	To                  string   `json:"to"`
	Label               string   `json:"label"`
	Async               bool     `json:"async"`
	Count               int      `json:"count"`
	SpanIDs             []string `json:"spanIds"` // members in start-time order
	TotalDurationMicros int64    `json:"totalDurationMicros"`
	SpanMicros          int64    `json:"spanMicros"` // wall-clock: max(end) - min(start)
	MinDurationMicros   int64    `json:"minDurationMicros"`
	MaxDurationMicros   int64    `json:"maxDurationMicros"`
	AvgDurationMicros   int64    `json:"avgDurationMicros"`
	ErrorCount          int      `json:"errorCount"` // never silently folded away
	Concurrent          bool     `json:"concurrent"` // overlapping (fan-out) vs strictly sequential (N+1)
}

type Activation struct {
	Participant string `json:"participant"` // the callee lifeline the bar sits on
	SpanID      string `json:"spanId"`
	FromRow     int    `json:"fromRow"`
	ToRow       int    `json:"toRow"`
	Depth       int    `json:"depth"`
	Status      string `json:"status"`
}

type SequenceRow struct {
	Kind    string          `json:"kind"` // "call" | "return"
	Message *LaidOutMessage `json:"message"`
}

type SequenceLayout struct {
	Participants []string          `json:"participants"`
	Externals    []string          `json:"externals"`
	Rows         []SequenceRow     `json:"rows"` // row index → y in the renderer
	Activations  []Activation      `json:"activations"`
	Messages     []*LaidOutMessage `json:"messages"` // same objects as in Rows, in call order
	Groups       []*SequenceGroup  `json:"groups"`
}

const groupMin = 3 // fewer reads fine as individual rows; don't fold pairs

func sig(m Message) string {
	a := "s"
	if m.Async {
		a = "a"
	}
	return strings.Join([]string{m.From, m.To, m.Label, a}, "\x00")
}

func laidOrder(a, b *LaidOutMessage) int {
	return cmp.Or(cmp.Compare(a.StartTime, b.StartTime), cmp.Compare(a.SpanID, b.SpanID))
}

func msgEnd(m *LaidOutMessage) int64 { return m.StartTime + m.DurationMicros }

func detectGroups(ordered []*LaidOutMessage) []*SequenceGroup {
	// A message is a non-leaf if it strictly contains another (equal intervals contain neither).
	isLeaf := make([]bool, len(ordered))
	for i, m := range ordered {
		isLeaf[i] = true
		for j, n := range ordered {
			if i == j {
				continue
			}
			contains := m.StartTime <= n.StartTime && msgEnd(n) <= msgEnd(m)
			strict := m.StartTime < n.StartTime || msgEnd(n) < msgEnd(m)
			if contains && strict {
				isLeaf[i] = false
				break
			}
		}
	}

	groups := []*SequenceGroup{}
	// Walk maximal runs of consecutive leaves; within each run bucket by signature so an
	// interleaved cluster (dept/emp/dept/emp…) still yields one group per distinct call.
	for i := 0; i < len(ordered); {
		if !isLeaf[i] {
			i++
			continue
		}
		j := i
		for j < len(ordered) && isLeaf[j] {
			j++
		}
		buckets := map[string][]*LaidOutMessage{}
		var keys []string
		for _, m := range ordered[i:j] {
			k := sig(m.Message)
			if _, ok := buckets[k]; !ok {
				keys = append(keys, k)
			}
			buckets[k] = append(buckets[k], m)
		}
		for _, k := range keys {
			if members := buckets[k]; len(members) >= groupMin {
				groups = append(groups, summarizeGroup(members))
			}
		}
		i = j
	}
	return groups
}

func summarizeGroup(members []*LaidOutMessage) *SequenceGroup {
	sorted := slices.Clone(members)
	slices.SortStableFunc(sorted, laidOrder)
	lead := sorted[0]
	g := &SequenceGroup{
		ID: lead.SpanID, Signature: sig(lead.Message), From: lead.From, To: lead.To, Label: lead.Label,
		Async: lead.Async, Count: len(sorted), SpanIDs: make([]string, len(sorted)),
		MinDurationMicros: sorted[0].DurationMicros, MaxDurationMicros: sorted[0].DurationMicros,
	}
	wallStart, wallEnd := sorted[0].StartTime, msgEnd(sorted[0])
	var maxEnd int64
	for i, m := range sorted {
		g.SpanIDs[i] = m.SpanID
		g.TotalDurationMicros += m.DurationMicros
		g.MinDurationMicros = min(g.MinDurationMicros, m.DurationMicros)
		g.MaxDurationMicros = max(g.MaxDurationMicros, m.DurationMicros)
		wallStart = min(wallStart, m.StartTime)
		wallEnd = max(wallEnd, msgEnd(m))
		if m.Status == "error" {
			g.ErrorCount++
		}
		// Concurrent if any member starts before an earlier member has finished.
		if i > 0 && m.StartTime < maxEnd {
			g.Concurrent = true
		}
		if i == 0 {
			maxEnd = msgEnd(m)
		} else {
			maxEnd = max(maxEnd, msgEnd(m))
		}
	}
	g.SpanMicros = wallEnd - wallStart
	g.AvgDurationMicros = int64(jsRound(float64(g.TotalDurationMicros) / float64(g.Count)))
	return g
}

// LayoutSequence lays out a sequence. expanded = group ids the viewer has opened; every other
// group renders collapsed (the default).
func LayoutSequence(sequence Sequence, expanded map[string]bool) SequenceLayout {
	sorted := make([]*LaidOutMessage, len(sequence.Messages))
	for i, m := range sequence.Messages {
		sorted[i] = &LaidOutMessage{Message: m, CallRow: -1, ReturnRow: -1}
	}
	slices.SortStableFunc(sorted, laidOrder)

	groups := detectGroups(sorted)

	// A collapsed group becomes one synthetic call spanning its wall-clock window; an expanded
	// group keeps its members (its lead row carries the group so the renderer can offer
	// "collapse"). Non-grouped messages pass through untouched.
	memberOf := map[string]*SequenceGroup{}
	for _, g := range groups {
		for _, id := range g.SpanIDs {
			memberOf[id] = g
		}
	}
	emitted := map[string]bool{}
	ordered := make([]*LaidOutMessage, 0, len(sorted))
	for _, m := range sorted {
		g := memberOf[m.SpanID]
		switch {
		case g == nil:
			ordered = append(ordered, m)
		case expanded[g.ID]:
			m.Group = g
			m.GroupRole = "member"
			if m.SpanID == g.ID {
				m.GroupRole = "lead"
			}
			ordered = append(ordered, m)
		case !emitted[g.ID]:
			// We iterate in start order, so the first member reached is the lead.
			emitted[g.ID] = true
			synthetic := *m
			synthetic.SpanID = g.ID
			synthetic.DurationMicros = g.SpanMicros
			if g.ErrorCount > 0 {
				synthetic.Status = "error"
			}
			synthetic.Group = g
			synthetic.GroupRole = "collapsed"
			ordered = append(ordered, &synthetic)
		}
	}
	slices.SortStableFunc(ordered, laidOrder)

	type openCall struct {
		m   *LaidOutMessage
		end int64
	}
	rows := []SequenceRow{}
	activations := []Activation{}
	var open []openCall

	closeTop := func() {
		finished := open[len(open)-1]
		open = open[:len(open)-1]
		finished.m.ReturnRow = len(rows)
		activations = append(activations, Activation{
			Participant: finished.m.To, SpanID: finished.m.SpanID, FromRow: finished.m.CallRow,
			ToRow: len(rows), Depth: finished.m.Depth, Status: finished.m.Status,
		})
		rows = append(rows, SequenceRow{Kind: "return", Message: finished.m})
	}

	for _, m := range ordered {
		// Close any open activation that finished before this call starts (a sibling that
		// already returned), innermost first.
		for len(open) > 0 && open[len(open)-1].end <= m.StartTime {
			closeTop()
		}
		// Lane offset: how many activations are already open on this callee's lifeline.
		m.Depth = 0
		for _, o := range open {
			if o.m.To == m.To {
				m.Depth++
			}
		}
		m.CallRow = len(rows)
		rows = append(rows, SequenceRow{Kind: "call", Message: m})
		if m.Async {
			m.ReturnRow = m.CallRow // fire-and-forget: no return arrow, no activation bar
		} else {
			open = append(open, openCall{m, m.StartTime + m.DurationMicros})
		}
	}
	// Close whatever is still open, innermost first (LIFO), so returns nest correctly.
	for len(open) > 0 {
		closeTop()
	}

	return SequenceLayout{
		Participants: sequence.Participants, Externals: sequence.Externals,
		Rows: rows, Activations: activations, Messages: ordered, Groups: groups,
	}
}
