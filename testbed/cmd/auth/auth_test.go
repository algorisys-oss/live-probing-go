package main

import (
	"strings"
	"testing"
	"time"
)

// Fixtures produced by Node (crypto.scryptSync / an HS256 token in jsonwebtoken's layout), so a
// Go auth service accepts users and sessions created by the TS one — and vice versa.
const (
	nodeHash = "00112233445566778899aabbccddeeff:7b052cef88fa90f0538e05097ff63183bb6d601da2543868545d7e89d2894fc30d8ba43cbf5986a756a7768fbe5aa3b44d5a496bb9f9068c7308a13465f2ca08"
	nodeJWT  = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1LTEiLCJqdGkiOiJqLTEiLCJpYXQiOjE3MDAwMDAwMDAsImV4cCI6NDEwMjQ0NDgwMH0.oGvgu1BRqxuojqBlMkpIBoQkDDhIoGQok8DzKJq3iNs"
)

func TestVerifiesNodeScryptHash(t *testing.T) {
	if !verifyPassword("password123", nodeHash) {
		t.Error("a hash written by the TS auth service must verify")
	}
	if verifyPassword("wrong", nodeHash) || verifyPassword("password123", "garbage") {
		t.Error("wrong password / malformed hash must not verify")
	}
}

func TestHashRoundTrip(t *testing.T) {
	h := hashPassword("s3cret!")
	if parts := strings.Split(h, ":"); len(parts) != 2 || len(parts[0]) != 32 || len(parts[1]) != 128 {
		t.Errorf("format salt(16B hex):hash(64B hex), got %q", h)
	}
	if !verifyPassword("s3cret!", h) || verifyPassword("nope", h) {
		t.Error("round trip")
	}
}

func TestVerifiesNodeJWT(t *testing.T) {
	claims, ok := verifyJWT(nodeJWT, "dev-secret-change-me", time.Unix(1_800_000_000, 0))
	if !ok || claims.Sub != "u-1" || claims.Jti != "j-1" {
		t.Errorf("claims %+v ok=%v", claims, ok)
	}
	if _, ok := verifyJWT(nodeJWT, "other-secret", time.Unix(1_800_000_000, 0)); ok {
		t.Error("wrong secret must fail")
	}
	if _, ok := verifyJWT(nodeJWT, "dev-secret-change-me", time.Unix(4_200_000_000, 0)); ok {
		t.Error("expired token must fail")
	}
}

func TestJWTRoundTripAndTamper(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok := signJWT(jwtClaims{Sub: "user", Jti: "id"}, "k", now, time.Hour)
	if c, ok := verifyJWT(tok, "k", now.Add(time.Minute)); !ok || c.Sub != "user" {
		t.Errorf("round trip: %+v %v", c, ok)
	}
	parts := strings.Split(tok, ".")
	if _, ok := verifyJWT(parts[0]+"."+parts[0]+"."+parts[2], "k", now); ok {
		t.Error("tampered payload must fail")
	}
	if _, ok := verifyJWT("not.a.jwt.at.all", "k", now); ok {
		t.Error("malformed token must fail")
	}
}
