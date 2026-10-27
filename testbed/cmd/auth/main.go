// Command auth: signup / login / token verification. Users live in Postgres, sessions in Redis
// (Redis is the source of truth for validity, which makes sessions revocable and puts a Redis hop
// on every authenticated request's trace).
package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"shopwave/shared"
)

const sessionTTL = time.Hour

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);`

// Known accounts so the API can be used without signing up first; all share one password.
var sampleUsers = []string{"alice@shopwave.test", "bob@shopwave.test", "carol@shopwave.test"}

const samplePassword = "password123"

func main() {
	ctx := context.Background()
	defer shared.InitTracing(ctx, "auth")()
	secret := shared.Env("JWT_SECRET", "dev-secret-change-me")
	pool := shared.NewPool(ctx, shared.Env("POSTGRES_DB", "auth"))
	shared.MustExec(ctx, pool, schema)
	for _, email := range sampleUsers { // idempotent: real signups are untouched
		shared.MustExec(ctx, pool, "INSERT INTO users (email, password_hash) VALUES ($1, $2) ON CONFLICT (email) DO NOTHING", email, hashPassword(samplePassword))
	}
	rdb := shared.NewRedis()

	issue := func(ctx context.Context, userID string) (string, error) {
		jti := newUUID()
		if err := rdb.Set(ctx, "session:"+jti, userID, sessionTTL).Err(); err != nil {
			return "", err
		}
		return signJWT(jwtClaims{Sub: userID, Jti: jti}, secret, time.Now(), sessionTTL), nil
	}

	s := shared.NewServer("auth")
	s.Handle("POST /signup", func(w http.ResponseWriter, r *http.Request) error {
		var body map[string]any
		if err := shared.ReadJSON(r, &body); err != nil {
			return err
		}
		email, _ := body["email"].(string)
		password, _ := body["password"].(string)
		if !strings.Contains(email, "@") || len(password) < 6 {
			return shared.Fail(400, "invalid_input")
		}
		var userID string
		err := pool.QueryRow(r.Context(), "INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id::text", email, hashPassword(password)).Scan(&userID)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return shared.Fail(409, "email_taken")
		}
		if err != nil {
			return err
		}
		token, err := issue(r.Context(), userID)
		if err != nil {
			return err
		}
		return shared.WriteJSON(w, 201, map[string]string{"token": token, "userId": userID})
	})

	s.Handle("POST /login", func(w http.ResponseWriter, r *http.Request) error {
		var body map[string]any
		if err := shared.ReadJSON(r, &body); err != nil {
			return shared.Fail(401, "invalid_credentials")
		}
		email, ok1 := body["email"].(string)
		password, ok2 := body["password"].(string)
		if !ok1 || !ok2 {
			return shared.Fail(401, "invalid_credentials")
		}
		var userID, hash string
		err := pool.QueryRow(r.Context(), "SELECT id::text, password_hash FROM users WHERE email = $1", email).Scan(&userID, &hash)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !verifyPassword(password, hash)) {
			return shared.Fail(401, "invalid_credentials")
		}
		if err != nil {
			return err
		}
		token, err := issue(r.Context(), userID)
		if err != nil {
			return err
		}
		return shared.WriteJSON(w, 200, map[string]string{"token": token, "userId": userID})
	})

	s.Handle("GET /verify", func(w http.ResponseWriter, r *http.Request) error {
		token, ok := strings.CutPrefix(r.Header.Get("authorization"), "Bearer ")
		if !ok {
			return shared.Fail(401, "unauthorized")
		}
		claims, ok := verifyJWT(token, secret, time.Now())
		if !ok || claims.Sub == "" || claims.Jti == "" {
			return shared.Fail(401, "unauthorized")
		}
		stored, err := rdb.Get(r.Context(), "session:"+claims.Jti).Result()
		if errors.Is(err, redis.Nil) || (err == nil && stored != claims.Sub) {
			return shared.Fail(401, "unauthorized")
		}
		if err != nil {
			return err
		}
		return shared.WriteJSON(w, 200, map[string]string{"userId": claims.Sub})
	})

	s.Start(shared.EnvInt("AUTH_PORT", 3001), func() {
		pool.Close()
		_ = rdb.Close()
	})
}
