package core

// PII / secret masking at the narrow waist. Every ingest path runs events through here before
// they are buffered, persisted (SQLite) and rendered (UI). Nothing downstream re-inspects
// payloads for sensitive data, so this is the place to scrub it.
//
// Two layers of defense, both conservative to avoid leaking:
//  1. Key-based — attribute keys whose name signals a secret/PII have their value replaced
//     wholesale (string, number and boolean values alike).
//  2. Value-based — any string (attribute value or identity field) is scanned for embedded
//     emails, bearer tokens, and Luhn-valid card numbers, which are replaced inline.

import (
	"regexp"
	"strings"
)

type MaskOptions struct {
	Placeholder        string   // replacement token; default "[redacted]"
	RedactKeys         []string // extra attribute-key tokens (case-insensitive) to redact
	SkipIdentityFields bool     // don't scan operation / peer / participant (default: scan them)
}

const defaultPlaceholder = "[redacted]"

// Key tokens matched as a whole word/segment. Kept exact to avoid false positives like
// "author" → "auth" or "panel" → "pan".
var sensitiveParts = map[string]bool{
	"pwd": true, "auth": true, "ssn": true, "sin": true, "cvv": true, "cvc": true, "pin": true,
	"otp": true, "email": true, "phone": true, "mobile": true, "dob": true, "iban": true,
	"pan": true, "card": true, "passport": true, "aadhaar": true,
}

// Key tokens matched as a substring — specific enough that a substring hit is a true positive.
var sensitiveContains = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key", "authorization",
	"credential", "cardnumber", "accountnumber", "taxid",
}

var (
	emailRe  = regexp.MustCompile(`(?i)[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}`)
	bearerRe = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
	// Candidate card: 13–19 digits, optionally grouped by single spaces/hyphens. Confirmed by Luhn.
	cardRe   = regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)
	keySplit = regexp.MustCompile(`[^a-z0-9]+`)
)

func luhnValid(candidate string) bool {
	digits := make([]byte, 0, len(candidate))
	for i := 0; i < len(candidate); i++ {
		if c := candidate[i]; c >= '0' && c <= '9' {
			digits = append(digits, c)
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

func isSensitiveKey(key string, extra map[string]bool) bool {
	lower := strings.ToLower(key)
	for _, token := range sensitiveContains {
		if strings.Contains(lower, token) {
			return true
		}
	}
	for token := range extra {
		if strings.Contains(lower, token) {
			return true
		}
	}
	for _, part := range keySplit.Split(lower, -1) {
		if part != "" && (sensitiveParts[part] || extra[part]) {
			return true
		}
	}
	return false
}

// MaskString redacts PII patterns embedded in a free-text string. An empty placeholder means
// the default.
func MaskString(value, placeholder string) string {
	if value == "" {
		return value
	}
	if placeholder == "" {
		placeholder = defaultPlaceholder
	}
	value = emailRe.ReplaceAllLiteralString(value, placeholder)
	value = bearerRe.ReplaceAllLiteralString(value, placeholder)
	return cardRe.ReplaceAllStringFunc(value, func(m string) string {
		if luhnValid(m) {
			return placeholder
		}
		return m
	})
}

// MaskPII returns a masked copy of an event. Never mutates the input.
func MaskPII(event Event, opts MaskOptions) Event {
	placeholder := opts.Placeholder
	if placeholder == "" {
		placeholder = defaultPlaceholder
	}
	extra := make(map[string]bool, len(opts.RedactKeys))
	for _, k := range opts.RedactKeys {
		extra[strings.ToLower(k)] = true
	}

	attributes := make(Attributes, len(event.Attributes))
	for k, v := range event.Attributes {
		if isSensitiveKey(k, extra) {
			attributes[k] = placeholder
		} else if s, ok := v.(string); ok {
			attributes[k] = MaskString(s, placeholder)
		} else {
			attributes[k] = v
		}
	}

	out := event
	out.Attributes = attributes
	if !opts.SkipIdentityFields {
		out.Operation = MaskString(event.Operation, placeholder)
		out.Participant = MaskString(event.Participant, placeholder)
		out.Peer = MaskString(event.Peer, placeholder)
	}
	return out
}

// MaskEvents masks every event in a stream.
func MaskEvents(events []Event, opts MaskOptions) []Event {
	out := make([]Event, len(events))
	for i, e := range events {
		out[i] = MaskPII(e, opts)
	}
	return out
}
