package core

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Ingest and the adapters were specified in JavaScript and lean on its coercions. These
// reproduce the spec algorithms exactly where the output is observable (ids, labels, times).

var jsDecimal = regexp.MustCompile(`^[+-]?(?:Infinity|(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?)$`)

// ParseJSNumber is ECMAScript StringToNumber (what Number(string) does): surrounding whitespace
// is ignored, "" is 0, unsigned 0x/0o/0b integer literals are allowed, anything else that isn't
// a decimal literal or ±Infinity is NaN. (Go's ParseFloat differs: it accepts "inf", "nan" and
// hex floats, and rejects "0x10".)
func ParseJSNumber(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			n, ok := new(big.Int).SetString(s[2:], base)
			if !ok || strings.ContainsAny(s[2:], "_+-") {
				return math.NaN()
			}
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
	}
	if !jsDecimal.MatchString(s) {
		return math.NaN()
	}
	switch strings.TrimLeft(s, "+") {
	case "Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !strings.Contains(err.Error(), "value out of range") {
		return math.NaN()
	}
	return f // out-of-range parses saturate to ±Inf, as in JS
}

// FormatJSNumber is ECMAScript Number::toString(10): shortest round-trip digits, fixed notation
// for 1e-7 <= |x| < 1e21, exponent notation ("1e+21", "1.5e-7") outside it.
func FormatJSNumber(x float64) string {
	switch {
	case math.IsNaN(x):
		return "NaN"
	case math.IsInf(x, 1):
		return "Infinity"
	case math.IsInf(x, -1):
		return "-Infinity"
	case x == 0:
		return "0" // also -0
	}
	sign := ""
	if x < 0 {
		sign, x = "-", -x
	}
	// Shortest digits d1.d2...dk and exponent e, so x = 0.d1...dk × 10^n with n = e+1.
	m := strconv.FormatFloat(x, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(m, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	k, n := len(digits), exp+1

	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	e := n - 1
	es := "+" + strconv.Itoa(e)
	if e < 0 {
		es = strconv.Itoa(e)
	}
	if k == 1 {
		return sign + digits + "e" + es
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + es
}

// SaturatingMicros converts a float to int64 the way the TS code's numbers behave once clamped
// downstream: values beyond the int64 range saturate instead of wrapping (NaN → 0).
func SaturatingMicros(x float64) int64 {
	switch {
	case math.IsNaN(x):
		return 0
	case x >= math.MaxInt64:
		return math.MaxInt64
	case x <= math.MinInt64:
		return math.MinInt64
	}
	return int64(x)
}

// ToFixed is ECMAScript Number.prototype.toFixed: the exact binary value rounded to `digits`
// decimals, an exact tie going to the larger magnitude ("1.25"→"1.3" but 1.005→"1.00", because
// 1.005 is really 1.00499…). |x| >= 1e21 falls back to Number::toString, as in JS.
func ToFixed(x float64, digits int) string {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.Abs(x) >= 1e21 {
		return FormatJSNumber(x)
	}
	const prec = 512 // exact: a 53-bit mantissa times 10^digits (digits <= 100) fits easily
	scaled := new(big.Float).SetPrec(prec).SetFloat64(math.Abs(x))
	scaled.Mul(scaled, new(big.Float).SetPrec(prec).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)))
	n, _ := scaled.Int(nil) // truncates toward zero
	frac := new(big.Float).SetPrec(prec).Sub(scaled, new(big.Float).SetPrec(prec).SetInt(n))
	if frac.Cmp(big.NewFloat(0.5)) >= 0 {
		n.Add(n, big.NewInt(1))
	}
	s := n.String()
	if digits > 0 {
		if len(s) <= digits {
			s = strings.Repeat("0", digits-len(s)+1) + s
		}
		s = s[:len(s)-digits] + "." + s[len(s)-digits:]
	}
	if x < 0 {
		s = "-" + s // JS keeps the sign even when the result rounds to zero: (-0.0001).toFixed(2) = "-0.00"
	}
	return s
}
