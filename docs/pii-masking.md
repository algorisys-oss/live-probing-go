# PII / secret masking (`MaskPII`)

LiveProbe ships a masking helper that scrubs PII and secrets out of an `Event` before it is
buffered, persisted to SQLite, and rendered in the UI. Nothing downstream re-inspects payloads
for sensitive data, so masking happens **at the narrow waist**, the moment raw data becomes a
normalized `Event`.

- Code: [`internal/core/mask.go`](../internal/core/mask.go).
- Tests: `internal/core/mask_test.go`.
- Where it runs: on **every ingest** in the server (`/v1/traces` and `/v1/events`, so collector
  output is covered too), with the built-in ruleset. `MASK_PII=off` disables it, which is not
  recommended. See "Usage" below and the PII section of
  [integration-adapters.md](integration-adapters.md).

## API

```go
import "liveprobe/internal/core"

core.MaskPII(event core.Event, opts core.MaskOptions) core.Event        // masked copy of one event
core.MaskEvents(events []core.Event, opts core.MaskOptions) []core.Event // MaskPII over a stream
core.MaskString(value, placeholder string) string                       // value-level scrubber, standalone
```

```go
type MaskOptions struct {
	Placeholder        string   // replacement token; "" means "[redacted]"
	RedactKeys         []string // extra attribute-key tokens to treat as sensitive
	SkipIdentityFields bool     // don't scan operation/peer/participant (default: scan them)
}
```

The zero value `core.MaskOptions{}` is the built-in ruleset. `SkipIdentityFields` is the
inverse of the TS `maskIdentityFields` option, so that the zero value keeps the safe default.

`MaskPII` **never mutates its input.** It returns a new event with a new `Attributes` map.
`TraceID`, `SpanID`, `ParentSpanID`, `Kind`, `StartTime`, `Duration` and `Status` are copied
through untouched because they carry no free text.

## How it works: two layers of defense

PII hides in two places: the **name** of a field and the **value** of a field. `MaskPII`
handles both.

### Layer 1: key-based redaction

Every `Attributes` entry is checked by `isSensitiveKey(key)`. If the key *name* signals a
secret or PII, the whole value is replaced, regardless of type (so `cvv: 123`, a number, is
still redacted). A value already known to be sensitive is never scanned.

Matching balances recall against false positives in two ways:

- **Substring match** for tokens specific enough to be safe anywhere in the key:
  `password`, `passwd`, `secret`, `token`, `apikey` / `api_key`, `authorization`,
  `credential`, `cardnumber`, `accountnumber`, `taxid` (`sensitiveContains`).
- **Whole-segment match** for short or ambiguous tokens. The key is split on
  non-alphanumerics (`user.email` → `["user", "email"]`), and each segment is compared against
  `sensitiveParts`: `pwd`, `auth`, `ssn`, `sin`, `cvv`, `cvc`, `pin`, `otp`, `email`,
  `phone`, `mobile`, `dob`, `iban`, `pan`, `card`, `passport`, `aadhaar`.

The segment rule is why `auth` matches a key named `auth` but **not** `author`: `author`
is a single segment that isn't equal to `auth`. Caller-supplied `RedactKeys` are added to both
checks (lower-cased).

### Layer 2: value-based redaction (`MaskString`)

Some string values sit under an innocent-looking key. These values, and by default the identity
fields `Operation` / `Peer` / `Participant`, have the *text itself* scanned for embedded PII,
which is redacted inline:

| Pattern | Example matched | Notes |
|---|---|---|
| Email | `jane.doe@acme.com` | `-` is a valid local-part char, so `svc-carol@acme.com` is matched whole |
| Bearer token | `Bearer abc.def.ghi` | matches the `Bearer ` prefix + token |
| Card number | `4242 4242 4242 4242` | 13–19 digits, with spaces/hyphens allowed **between** digits |

The card rule is guarded by a **Luhn checksum**. A 13–19 digit run is only redacted if it
passes Luhn, the checksum that real card numbers satisfy. This is deliberate: a 16-digit
*order id* that fails Luhn is left intact, so the helper doesn't over-redact ordinary long
numbers. The card regex also keeps separators *between* digits only, so a trailing space
after a number isn't swallowed.

So `note: "ping bob@shop.io asap"` (innocent key, PII value) → `"ping [redacted] asap"`,
and a route `GET /users/jane@acme.com` in `Operation` → `GET /users/[redacted]`.

## Usage: a backstop, not the primary control

The server-side pass is the **catch-all**, not the whole strategy. An adapter should still
prefer an **allow-list**: copy only the known-safe keys out of the raw payload. Anything that
slips through is then scrubbed at ingest. An adapter that wants stricter rules than the server's
(custom placeholder, extra keys) can run `MaskEvents` itself before returning:

```go
func myAdapter(raw any) []core.Event {
	event := /* map raw → Event, copying only known-safe attribute keys */
	return core.MaskEvents([]core.Event{event}, core.MaskOptions{
		Placeholder: "***",
		RedactKeys:  []string{"name", "customer_id"},
	})
}

core.MaskPII(event, core.MaskOptions{SkipIdentityFields: true}) // leave operation/peer/participant alone
```

## Limitations (know these)

- Layer 2 covers **emails, bearer tokens, and Luhn-valid cards** only. Phone numbers,
  national IDs, and free-form names have no reliable value signature, so value scanning does
  **not** catch them. They rely on Layer 1 matching the key name (or on caller `RedactKeys` or
  an allow-list). The masking is conservative by design and does not guarantee that every PII
  value is caught.
- Go's regexp `\s` is ASCII-only (JavaScript's also matches Unicode spaces). This only affects
  the bearer-token pattern on exotic whitespace
  ([delta 6](migration-plan.md#known-semantic-deltas-deliberate-documented)).
- Masking is a copy-time transform on the `Event`. If a client already persists the raw
  payload elsewhere, that copy is out of scope: masking only protects what enters LiveProbe.
- The shipped adapters (`adapter-id-1`, `algo-instrumentation`) do not call it themselves.
  They rely on the server's ingest pass, so their verified output stays identical to the TS
  adapters.
