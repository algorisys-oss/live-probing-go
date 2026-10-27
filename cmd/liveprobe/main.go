// Command liveprobe runs the LiveProbe server: OTLP/native ingest, REST API, live /ws stream and
// (when PUBLIC_DIR is set) a static UI. Configuration is by environment, matching the TS server.
package main

import (
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"liveprobe/internal/server"
)

// numEnv returns a finite env number >= min, or ok=false (fall back to the server default).
func numEnv(name string, min float64) (float64, bool) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < min {
		return 0, false
	}
	return n, true
}

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

func main() {
	port := 4318
	if p, ok := numEnv("PORT", 0); ok {
		port = int(p)
	}
	// Persist history at DB_PATH (e.g. a mounted /data volume); default ./data/liveprobe.db.
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = filepath.Join("data", "liveprobe.db")
	}
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			log.Fatalf("liveprobe: cannot create db dir: %v", err)
		}
	}

	o := server.Options{
		DBPath: dbPath,
		// Bind loopback by default (the ingest/API have no auth). HOST=0.0.0.0 to expose.
		Host: os.Getenv("HOST"),
		// PUBLIC_DIR serves a built UI from disk (migration aid until the Go UI is embedded).
		PublicDir: os.Getenv("PUBLIC_DIR"),
		// MASK_PII=off disables the built-in PII/secret scrubbing (not recommended).
		MaskDisabled: os.Getenv("MASK_PII") == "off",
	}
	if n, ok := numEnv("RETENTION_DAYS", 0); ok {
		d := int(n)
		o.RetentionDays = &d
	}
	if n, ok := numEnv("LIVE_WINDOW_MINUTES", 0); ok {
		o.HorizonMicros = int64(n * 60 * 1_000_000)
	}
	if n, ok := numEnv("MAX_TRACES", 1); ok {
		o.MaxTraces = int(n)
	}
	for _, origin := range strings.Split(os.Getenv("CORS_ORIGINS"), ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			o.CORSOrigins = append(o.CORSOrigins, origin)
		}
	}
	// ALERT_WEBHOOK_URL: POST target fired once per inactive→active alert transition. Unset = off.
	if url := strings.TrimSpace(os.Getenv("ALERT_WEBHOOK_URL")); url != "" {
		hook := server.DefaultWebhookOptions(url)
		if n, ok := numEnv("ALERT_WEBHOOK_TIMEOUT_MS", 1); ok {
			hook.Timeout = ms(n)
		}
		if n, ok := numEnv("ALERT_WEBHOOK_RETRIES", 0); ok {
			hook.Retries = int(n)
		}
		if n, ok := numEnv("ALERT_WEBHOOK_COOLDOWN_MS", 0); ok {
			hook.Cooldown = ms(n)
		}
		hook.OnSent = func(a server.FiringAlert) { log.Printf("alert webhook sent: %s", a.ID) }
		hook.OnError = func(err error, a server.FiringAlert) { log.Printf("alert webhook failed for %s: %v", a.ID, err) }
		o.AlertWebhook = &hook
	}

	srv, err := server.New(o)
	if err != nil {
		log.Fatalf("liveprobe: %v", err)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		srv.Close()
	}()

	host := o.Host
	if host == "" {
		host = "127.0.0.1"
	}
	log.Printf("liveprobe server listening on %s:%d (OTLP ingest POST /v1/traces, ws /ws)", host, port)
	if err := srv.ListenAndServe(port); err != nil {
		log.Fatalf("liveprobe: %v", err)
	}
	srv.Close()
}
