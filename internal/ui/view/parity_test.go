package view

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"liveprobe/internal/core"
)

// View-logic differential, Go side (see tools/parity/view-parity.mts). Skipped unless
// LIVEPROBE_PARITY_DIR is set. Output mirrors the TS shapes so compare.mjs can diff them.
func TestViewParityWithTypeScript(t *testing.T) {
	dir := os.Getenv("LIVEPROBE_PARITY_DIR")
	if dir == "" {
		t.Skip("LIVEPROBE_PARITY_DIR not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "view-input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		Topologies []core.Topology `json:"topologies"`
		Micros     []float64       `json:"micros"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	type xy struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	type cell struct {
		From              string  `json:"from"`
		To                string  `json:"to"`
		Calls             int     `json:"calls"`
		Errors            int     `json:"errors"`
		ErrorRate         float64 `json:"errorRate"`
		AvgDurationMicros int64   `json:"avgDurationMicros"`
		Health            Health  `json:"health"`
	}
	var topos []any
	for _, tp := range in.Topologies {
		l := ComputeLayout(tp.Nodes, tp.Edges)
		pos := map[string]xy{}
		for n, p := range l.Positions {
			pos[n] = xy{p.X, p.Y}
		}
		h := TopologyHealth(tp, DefaultThresholds)
		m := DependencyMatrix(tp, DefaultThresholds)
		cells := make([]cell, len(m.Cells))
		for i, c := range m.Cells {
			cells[i] = cell{c.From, c.To, c.Calls, c.Errors, c.ErrorRate, c.AvgDurationMicros, c.Health}
		}
		topos = append(topos, map[string]any{
			"layout": map[string]any{"positions": pos, "width": l.Width, "height": l.Height},
			"health": map[string]any{"edgeHealth": h.EdgeHealth, "nodeHealth": h.NodeHealth, "medianLatencyMicros": h.MedianLatencyMicros},
			"matrix": map[string]any{"rows": m.Rows, "cols": m.Cols, "cells": cells, "medianLatencyMicros": m.MedianLatencyMicros},
		})
	}
	micros := make([]string, len(in.Micros))
	for i, m := range in.Micros {
		micros[i] = FormatMicros(m)
	}
	data, _ := json.Marshal(map[string]any{"topologies": topos, "micros": micros})
	if err := os.WriteFile(filepath.Join(dir, "view-go.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
