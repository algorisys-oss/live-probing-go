package core

import (
	"cmp"
	"slices"
)

type Message struct {
	From           string `json:"from"`
	To             string `json:"to"`
	SpanID         string `json:"spanId"` // the span this arrow represents (links to the waterfall row)
	Label          string `json:"label"`
	StartTime      int64  `json:"startTime"`
	DurationMicros int64  `json:"durationMicros"`
	Status         string `json:"status"`
	Async          bool   `json:"async"`
}

type Sequence struct {
	Participants []string  `json:"participants"`
	Messages     []Message `json:"messages"`
	// Participants known only as a peer (nothing instrumented on their side) — "ghost"
	// lifelines. Datastores and real participants of the trace are not listed.
	Externals []string `json:"externals"`
}

func eventOrder(a, b Event) int {
	return cmp.Or(cmp.Compare(a.StartTime, b.StartTime), cmp.Compare(a.SpanID, b.SpanID))
}

// SequenceFor projects one assembled trace into a time-ordered sequence. A span whose parent
// runs in a different participant is a request arrow (parent -> span). A span that calls a peer
// — a datastore or an uninstrumented "ghost" service — is an arrow to that peer, unless the
// callee reported its own span (then the parent/child arrow already covers the call).
// Same-participant internal work is not drawn.
func SequenceFor(trace *AssembledTrace) Sequence {
	byID := make(map[string]Event, len(trace.Events))
	realParticipants := map[string]bool{}
	for _, e := range trace.Events {
		byID[e.SpanID] = e
		realParticipants[e.Participant] = true
	}
	bridged := bridgedSpans(trace.Events, byID)

	ordered := slices.Clone(trace.Events)
	slices.SortStableFunc(ordered, eventOrder)

	participants := newOrderedSet()
	ghosts := newOrderedSet()
	messages := []Message{}
	for _, e := range ordered {
		parent, hasParent := byID[e.ParentSpanID]
		if hasParent && e.ParentSpanID != "" && parent.Participant != e.Participant {
			participants.add(parent.Participant)
			participants.add(e.Participant)
			messages = append(messages, Message{
				From: parent.Participant, To: e.Participant, SpanID: e.SpanID, Label: e.Operation,
				StartTime: e.StartTime, DurationMicros: e.Duration, Status: string(e.Status),
				Async: e.Kind == KindConsumer || parent.Kind == KindProducer,
			})
		} else if e.Peer != "" && e.Peer != e.Participant && !bridged[e.SpanID] {
			participants.add(e.Participant)
			participants.add(e.Peer)
			if !DatastoreSystems[e.Peer] && !realParticipants[e.Peer] {
				ghosts.add(e.Peer)
			}
			messages = append(messages, Message{
				From: e.Participant, To: e.Peer, SpanID: e.SpanID, Label: e.Operation,
				StartTime: e.StartTime, DurationMicros: e.Duration, Status: string(e.Status),
				Async: e.Kind == KindProducer || e.Kind == KindConsumer,
			})
		}
	}
	return Sequence{Participants: participants.items, Messages: messages, Externals: ghosts.items}
}
