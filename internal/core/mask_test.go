package core

import "testing"

// Port of packages/core/src/mask.test.ts.

func maskBase() Event {
	return Event{TraceID: "t1", SpanID: "s1", Participant: "orders-api", Operation: "GET /orders", Kind: KindServer, Status: StatusOK, Attributes: Attributes{}}
}

func TestMaskStringRedactsEmail(t *testing.T) {
	eq(t, MaskString("GET /users/jane.doe@acme.com", ""), "GET /users/[redacted]", "email")
}

func TestMaskStringRedactsLuhnCardOnly(t *testing.T) {
	eq(t, MaskString("card 4242 4242 4242 4242 done", ""), "card [redacted] done", "card")
	// 16 digits that fail Luhn stay put (e.g. an order id), so we don't over-redact.
	eq(t, MaskString("order 1234567812345678", ""), "order 1234567812345678", "non-luhn")
}

func TestMaskStringRedactsBearer(t *testing.T) {
	eq(t, MaskString("Authorization: Bearer abc.def.ghi", ""), "Authorization: [redacted]", "bearer")
}

func TestMaskStringLeavesCleanText(t *testing.T) {
	eq(t, MaskString("GET /orders/42", ""), "GET /orders/42", "clean")
}

func TestMaskPIIRedactsSensitiveKeys(t *testing.T) {
	in := maskBase()
	in.Attributes = Attributes{
		"password":      "hunter2",
		"api_key":       "sk-live-xyz",
		"authorization": "Bearer t",
		"user.email":    "a@b.com",
		"cvv":           float64(123),
		"order_id":      "ORD-1", // not sensitive
		"retries":       float64(3),
	}
	e := MaskPII(in, MaskOptions{})
	for _, k := range []string{"password", "api_key", "authorization", "user.email", "cvv"} {
		eq(t, e.Attributes[k], any("[redacted]"), k)
	}
	eq(t, e.Attributes["order_id"], any("ORD-1"), "order_id")
	eq(t, e.Attributes["retries"], any(float64(3)), "retries")
}

func TestMaskPIIRedactsValuesUnderNonSensitiveKey(t *testing.T) {
	in := maskBase()
	in.Attributes = Attributes{"note": "ping customer bob@shop.io asap"}
	eq(t, MaskPII(in, MaskOptions{}).Attributes["note"], any("ping customer [redacted] asap"), "note")
}

func TestMaskPIIMasksIdentityFields(t *testing.T) {
	in := maskBase()
	in.Operation, in.Peer = "GET /users/jane@acme.com", "carol@acme.com"
	e := MaskPII(in, MaskOptions{})
	eq(t, e.Operation, "GET /users/[redacted]", "operation")
	eq(t, e.Peer, "[redacted]", "peer")
	eq(t, e.Participant, "orders-api", "clean participant untouched")
}

func TestMaskPIICanSkipIdentityFields(t *testing.T) {
	in := maskBase()
	in.Operation = "GET /users/jane@acme.com"
	eq(t, MaskPII(in, MaskOptions{SkipIdentityFields: true}).Operation, "GET /users/jane@acme.com", "operation")
}

func TestMaskPIIExtraKeysAndPlaceholder(t *testing.T) {
	in := maskBase()
	in.Attributes = Attributes{"customer_name": "Jane Doe", "note": "ok"}
	e := MaskPII(in, MaskOptions{RedactKeys: []string{"name"}, Placeholder: "***"})
	eq(t, e.Attributes["customer_name"], any("***"), "customer_name")
	eq(t, e.Attributes["note"], any("ok"), "note")
}

func TestMaskPIIDoesNotMutateInput(t *testing.T) {
	in := maskBase()
	in.Attributes = Attributes{"password": "hunter2"}
	out := MaskPII(in, MaskOptions{})
	eq(t, in.Attributes["password"], any("hunter2"), "original untouched")
	eq(t, out.Attributes["password"], any("[redacted]"), "masked copy")
}

func TestMaskEvents(t *testing.T) {
	a, b := maskBase(), maskBase()
	a.Attributes = Attributes{"token": "x"}
	b.Attributes = Attributes{"pwd": "y"}
	out := MaskEvents([]Event{a, b}, MaskOptions{})
	eq(t, out[0].Attributes["token"], any("[redacted]"), "token")
	eq(t, out[1].Attributes["pwd"], any("[redacted]"), "pwd")
}
