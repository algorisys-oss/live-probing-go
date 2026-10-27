// Package collector bridges foreign telemetry into LiveProbe: a source (transport) feeds raw JSON
// documents to an adapter (format), which yields normalized events for a sink (POST /v1/events).
package collector

import (
	"math"
	"strconv"
	"strings"
	"time"

	"liveprobe/internal/core"
)

// The adapters were written against JavaScript's loose coercions; these helpers reproduce the
// ones they rely on so a raw event maps to exactly the same Event as in the TS collector.

// jsNumber mirrors Number(v) for decoded JSON. present=false (key missing) is NaN, null is 0.
func jsNumber(v any, present bool) float64 {
	if !present {
		return math.NaN()
	}
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		return core.ParseJSNumber(x)
	}
	return math.NaN() // objects/arrays (close enough: Number([5]) is 5, nobody sends that)
}

func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }

// jsRound mirrors Math.round (ties toward +Infinity), saturating at the int64 range.
func jsRound(x float64) int64 { return core.SaturatingMicros(math.Floor(x + 0.5)) }

// jsString mirrors String(v) for decoded JSON.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case float64:
		return core.FormatJSNumber(x)
	case bool:
		return strconv.FormatBool(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// Layouts Date.parse (V8) accepts that emitters plausibly send. Per the JS spec an ISO date-only
// string is UTC and an ISO date-time without an offset is local time; V8's non-ISO formats are
// local unless they carry a zone. V8's legacy heuristics for bare numbers ("200" → year 200,
// "12" → Dec 2001) are deliberately not reproduced: those parse to 0 here, and ingest clamps
// both outcomes to the same start time anyway.
var (
	zonedLayouts = []string{
		time.RFC3339Nano, "2006-01-02T15:04Z07:00",
		"2006-01-02T15:04:05.999999999-0700", "2006-01-02T15:04-0700", // Java-style offsets
		time.RFC1123, time.RFC1123Z, "Mon, 2 Jan 2006 15:04:05 MST",
		"Mon Jan 02 2006 15:04:05 GMT-0700", // Date#toString() (zone name suffix stripped)
	}
	utcLayouts   = []string{"2006-01-02", "2006-01", "2006"}
	localLayouts = []string{
		"2006-01-02T15:04:05.999999999", "2006-01-02T15:04", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04",
		"2006-1-2", "2006/01/02", "2006/01/02 15:04", "2006/01/02 15:04:05", "01/02/2006", "01/02/2006 15:04:05",
		"Jan 2 2006", "Jan 2, 2006", "Jan 2 2006 15:04:05", "Jan 2, 2006 15:04:05", "Jan 2, 2006 15:04",
		"2 Jan 2006", "2 Jan 2006 15:04", "2 Jan 2006 15:04:05",
	}
)

// dateParse mirrors Date.parse: epoch milliseconds, ok=false when unparseable.
func dateParse(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		s = s[:i] // "… GMT+0530 (India Standard Time)"
	}
	for _, l := range zonedLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	for _, l := range utcLayouts { // before the local layouts: "2006-1-2" would also match ISO dates
		if t, err := time.Parse(l, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	for _, l := range localLayouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

func isScalar(v any) bool {
	switch v.(type) {
	case string, float64, bool:
		return true
	}
	return false
}

// entries mirrors Object.entries for an object or array value (arrays yield index keys).
func entries(v any) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case []any:
		out := make(map[string]any, len(x))
		for i, e := range x {
			out[strconv.Itoa(i)] = e
		}
		return out
	}
	return nil
}
