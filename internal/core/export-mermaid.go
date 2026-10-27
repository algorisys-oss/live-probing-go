package core

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	nonAlnum     = regexp.MustCompile(`[^A-Za-z0-9]`)
	leadingAlpha = regexp.MustCompile(`^[A-Za-z]`)
)

// Mermaid needs alphanumeric node ids; keep a readable alias for the label.
func mermaidNodeID(name string) string {
	cleaned := nonAlnum.ReplaceAllString(name, "_")
	if leadingAlpha.MatchString(cleaned) {
		return cleaned
	}
	return "n_" + cleaned
}

func mermaidLabel(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, `"`, "'"), "\n", " ")
}

func ToMermaidSequence(seq Sequence) string {
	lines := []string{"sequenceDiagram"}
	for _, p := range seq.Participants {
		lines = append(lines, "  participant "+mermaidNodeID(p)+" as "+mermaidLabel(p))
	}
	for _, m := range seq.Messages {
		arrow := "->>"
		if m.Async {
			arrow = "-)"
		}
		lines = append(lines, "  "+mermaidNodeID(m.From)+arrow+mermaidNodeID(m.To)+": "+mermaidLabel(m.Label))
	}
	return strings.Join(lines, "\n")
}

func ToMermaidFlow(topo Topology) string {
	lines := []string{"graph LR"}
	for _, n := range topo.Nodes {
		lines = append(lines, "  "+mermaidNodeID(n)+`["`+mermaidLabel(n)+`"]`)
	}
	for _, e := range topo.Edges {
		label := strconv.Itoa(e.Calls)
		if e.Errors > 0 {
			label += " (" + strconv.Itoa(e.Errors) + " err)"
		}
		lines = append(lines, "  "+mermaidNodeID(e.From)+" -->|"+label+"| "+mermaidNodeID(e.To))
	}
	// Uninstrumented ("ghost") peers render dashed.
	if len(topo.Externals) > 0 {
		lines = append(lines, "  classDef external stroke-dasharray: 6 4,opacity:0.75")
		for _, ext := range topo.Externals {
			lines = append(lines, "  class "+mermaidNodeID(ext)+" external")
		}
	}
	return strings.Join(lines, "\n")
}
