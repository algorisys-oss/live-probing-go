package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
)

// Password hashing compatible with the TS service: Node's scryptSync defaults (N=16384, r=8,
// p=1), a 16-byte random salt, a 64-byte key, stored as "saltHex:hashHex".
const (
	keyLen  = 64
	saltLen = 16
)

func hashPassword(password string) string {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key, err := scrypt.Key([]byte(password), salt, 16384, 8, 1, keyLen)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(key)
}

func verifyPassword(password, stored string) bool {
	saltHex, hashHex, ok := strings.Cut(stored, ":")
	if !ok || saltHex == "" || hashHex == "" {
		return false
	}
	salt, err1 := hex.DecodeString(saltHex)
	expected, err2 := hex.DecodeString(hashHex)
	if err1 != nil || err2 != nil {
		return false
	}
	actual, err := scrypt.Key([]byte(password), salt, 16384, 8, 1, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(expected, actual) == 1
}

// HS256 JWTs in jsonwebtoken's layout ({"alg":"HS256","typ":"JWT"} . {sub,jti,iat,exp}), so tokens
// issued by either implementation verify in the other. Small enough not to need a JWT library.

type jwtClaims struct {
	Sub string `json:"sub"`
	Jti string `json:"jti"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

var b64 = base64.RawURLEncoding

func jwtSignature(signingInput, secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}

func signJWT(c jwtClaims, secret string, now time.Time, ttl time.Duration) string {
	c.Iat, c.Exp = now.Unix(), now.Add(ttl).Unix()
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload, _ := json.Marshal(c)
	input := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	return input + "." + b64.EncodeToString(jwtSignature(input, secret))
}

// verifyJWT checks the HS256 signature and expiry; ok=false for anything malformed.
func verifyJWT(token, secret string, now time.Time) (jwtClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, false
	}
	var header struct {
		Alg string `json:"alg"`
	}
	raw, err := b64.DecodeString(parts[0])
	if err != nil || json.Unmarshal(raw, &header) != nil || header.Alg != "HS256" {
		return jwtClaims{}, false
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, jwtSignature(parts[0]+"."+parts[1], secret)) {
		return jwtClaims{}, false
	}
	var c jwtClaims
	if raw, err = b64.DecodeString(parts[1]); err != nil || json.Unmarshal(raw, &c) != nil {
		return jwtClaims{}, false
	}
	if c.Exp != 0 && now.Unix() >= c.Exp {
		return jwtClaims{}, false
	}
	return c, true
}

// newUUID is a random (v4) UUID for session ids.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
