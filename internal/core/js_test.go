package core

import (
	"math"
	"testing"
)

func TestParseJSNumber(t *testing.T) {
	cases := map[string]float64{
		"": 0, "  ": 0, " 200 ": 200, "1e3": 1000, "1.": 1, ".5": 0.5, "-2.5": -2.5, "+7": 7,
		"0x10": 16, "0X1f": 31, "0o17": 15, "0b101": 5, "1e400": math.Inf(1),
	}
	for in, want := range cases {
		if got := ParseJSNumber(in); got != want {
			t.Errorf("ParseJSNumber(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"Infinity", "+Infinity"} {
		if !math.IsInf(ParseJSNumber(in), 1) {
			t.Errorf("%q should be +Inf", in)
		}
	}
	// JS rejects what Go's ParseFloat accepts, and vice versa.
	for _, in := range []string{"abc", "inf", "nan", "NaN", "0x1p4", "-0x10", "1_000", "0x", "1e", ".", "12px", "infinity"} {
		if !math.IsNaN(ParseJSNumber(in)) {
			t.Errorf("ParseJSNumber(%q) = %v, want NaN", in, ParseJSNumber(in))
		}
	}
}

func TestFormatJSNumber(t *testing.T) {
	a, b := 0.1, 0.2 // runtime floats: a Go constant 0.1+0.2 is folded exactly to 0.3
	cases := map[float64]string{
		a + b: "0.30000000000000004",
		0:     "0", 1: "1", -1.5: "-1.5", 123.456: "123.456", 1e20: "100000000000000000000", 1e21: "1e+21",
		1.5e21: "1.5e+21", 1e-6: "0.000001", 1e-7: "1e-7", 1.25e-8: "1.25e-8",
		1700000000000: "1700000000000", 407.52: "407.52", math.Inf(-1): "-Infinity",
	}
	for in, want := range cases {
		if got := FormatJSNumber(in); got != want {
			t.Errorf("FormatJSNumber(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestSaturatingMicros(t *testing.T) {
	if SaturatingMicros(1e24) != math.MaxInt64 || SaturatingMicros(-1e24) != math.MinInt64 || SaturatingMicros(math.NaN()) != 0 || SaturatingMicros(42.9) != 42 {
		t.Error("saturation")
	}
}

func TestToFixed(t *testing.T) {
	// Expected values captured from Node (x.toFixed(d)).
	cases := []struct {
		x    float64
		d    int
		want string
	}{
		{1.005, 2, "1.00"}, {1.25, 1, "1.3"}, {2.5, 0, "3"}, {-1.25, 1, "-1.3"}, {0.000001, 7, "0.0000010"},
		{-0.0001, 2, "-0.00"}, {123.456, 1, "123.5"}, {8.345, 2, "8.35"}, {1.45, 1, "1.4"}, {0.5, 0, "1"},
		{1.5, 0, "2"}, {10.235, 2, "10.23"}, {1e21, 2, "1e+21"}, {99.95, 1, "100.0"}, {0.05, 1, "0.1"},
	}
	for _, c := range cases {
		if got := ToFixed(c.x, c.d); got != c.want {
			t.Errorf("ToFixed(%v, %d) = %q, want %q", c.x, c.d, got, c.want)
		}
	}
}
