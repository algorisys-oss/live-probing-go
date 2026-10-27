package shared

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// --- Postgres --------------------------------------------------------------------------------

// sqlSpanName names a query span like the Node pg instrumentation's "pg.query:SELECT": the
// leading SQL keyword, so LiveProbe groups queries by kind rather than by full statement text.
func sqlSpanName(stmt string) string {
	fields := strings.Fields(stmt)
	if len(fields) == 0 {
		return "pg.query"
	}
	return "pg.query:" + strings.ToUpper(fields[0])
}

// NewPool opens a pgx pool for one database (one Postgres container, a database per service),
// with every query traced as a client span carrying db.system.name=postgresql. It retries the
// first connection: services boot before Postgres accepts connections.
func NewPool(ctx context.Context, database string) *pgxpool.Pool {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(Env("POSTGRES_USER", "shopwave"), Env("POSTGRES_PASSWORD", "shopwave")),
		Host:   Env("POSTGRES_HOST", "localhost") + ":" + Env("POSTGRES_PORT", "5432"),
		Path:   "/" + database,
	}
	cfg, err := pgxpool.ParseConfig(u.String() + "?sslmode=disable&pool_max_conns=10")
	if err != nil {
		log.Fatalf("postgres config: %v", err)
	}
	// No pool.acquire spans: the Node pg instrumentation had none, and they are noise on the diagram.
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithSpanNameFunc(sqlSpanName), otelpgx.WithDisableAcquireTracer())
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatalf("postgres pool: %v", err)
	}
	for i := 1; ; i++ {
		if err = pool.Ping(ctx); err == nil {
			return pool
		}
		if i == 30 {
			log.Fatalf("postgres %s not reachable: %v", database, err)
		}
		time.Sleep(time.Second)
	}
}

// --- Redis -----------------------------------------------------------------------------------

// NewRedis connects to REDIS_URL with every command traced (db.system=redis).
func NewRedis() *redis.Client {
	opt, err := redis.ParseURL(Env("REDIS_URL", "redis://localhost:6379"))
	if err != nil {
		log.Fatalf("redis url: %v", err)
	}
	opt.MaxRetries = 5
	// go-redis >= 9.x sends CLIENT MAINT_NOTIFICATIONS (a Redis Enterprise feature) on every new
	// connection; OSS Redis rejects it and the rejection shows up as an errored span.
	opt.MaintNotificationsConfig = &maintnotifications.Config{Mode: maintnotifications.ModeDisabled}
	rdb := redis.NewClient(opt)
	if err := redisotel.InstrumentTracing(rdb); err != nil {
		log.Fatalf("redis tracing: %v", err)
	}
	return rdb
}

// MustExec runs schema/seed SQL at startup.
func MustExec(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		panic(fmt.Sprintf("startup sql failed: %v", err))
	}
}
