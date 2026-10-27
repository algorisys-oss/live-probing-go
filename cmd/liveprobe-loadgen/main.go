// Command liveprobe-loadgen posts synthetic multi-service traces to a LiveProbe server so the UI has
// something realistic to show without running the Docker testbed: a checkout flow (auth, a catalog
// N+1 against Postgres, a flaky order call that publishes to RabbitMQ for a payment worker), product
// browsing (Redis cache hit or Postgres miss) and logins. Occasional catalog slowdowns turn edges
// amber; order errors turn them red and fire alerts.
//
//	LIVEPROBE_URL=http://localhost:4319 TRACES_PER_SEC=6 ERROR_RATE=0.12 DURATION_SECONDS=0 liveprobe-loadgen
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"liveprobe/internal/core"
)

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v >= 0 {
		return v
	}
	return def
}

// tracer builds one trace's spans relative to a start time (all offsets/durations in µs).
type tracer struct {
	id     string
	start  int64
	events []core.Event
}

func (t *tracer) span(id, parent, who, peer, op string, kind core.SpanKind, off, dur int64, failed bool, attrs core.Attributes) string {
	spanID := t.id + "-" + id
	if parent != "" {
		parent = t.id + "-" + parent
	}
	status := core.StatusOK
	if failed {
		status = core.StatusError
	}
	if attrs == nil {
		attrs = core.Attributes{}
	}
	t.events = append(t.events, core.Event{TraceID: t.id, SpanID: spanID, ParentSpanID: parent, Participant: who, Peer: peer,
		Operation: op, Kind: kind, StartTime: t.start + off, Duration: dur, Status: status, Attributes: attrs})
	return id
}

func jitter(base int64) int64 { return base + rand.Int64N(base/2+1) - base/4 }

func checkout(t *tracer, errRate float64) {
	slow := rand.Float64() < 0.08 // catalog occasionally degrades → amber edge
	failed := rand.Float64() < errRate
	t.span("g", "", "gateway", "", "POST /api/checkout", core.KindServer, 0, 0, failed, core.Attributes{"http.method": "POST", "http.route": "/api/checkout"})
	cur := int64(1_000)
	auth := jitter(4_000)
	t.span("ga", "g", "gateway", "auth", "POST /verify", core.KindClient, cur, auth+800, false, nil)
	t.span("a", "ga", "auth", "", "POST /verify", core.KindServer, cur+400, auth, false, nil)
	t.span("ar", "a", "auth", "redis", "GET session", core.KindClient, cur+600, auth/3, false, core.Attributes{"db.system": "redis"})
	cur += auth + 1_500

	items := 2 + rand.IntN(4) // N+1: one SELECT per cart item
	q := jitter(2_500)
	if slow {
		q *= 6
	}
	cat := int64(items)*(q+500) + 1_000
	t.span("gc", "g", "gateway", "catalog", "GET /products", core.KindClient, cur, cat+800, false, nil)
	t.span("c", "gc", "catalog", "", "GET /products", core.KindServer, cur+400, cat, false, nil)
	for i := range items {
		t.span(fmt.Sprintf("q%d", i), "c", "catalog", "postgresql", "SELECT product", core.KindClient, cur+900+int64(i)*(q+500), q, false,
			core.Attributes{"db.system": "postgresql", "db.statement": "SELECT * FROM products WHERE id = $1"})
	}
	cur += cat + 1_500

	ord := jitter(18_000)
	t.span("go", "g", "gateway", "order", "POST /orders", core.KindClient, cur, ord+800, failed, nil)
	t.span("o", "go", "order", "", "POST /orders", core.KindServer, cur+400, ord, failed, core.Attributes{"http.status_code": statusCode(failed)})
	t.span("oi", "o", "order", "postgresql", "INSERT order", core.KindClient, cur+1_000, ord/3, false, core.Attributes{"db.system": "postgresql"})
	if !failed {
		pub := cur + 1_000 + ord/3 + 500
		t.span("op", "o", "order", "rabbitmq", "publish order.created", core.KindProducer, pub, 900, false, core.Attributes{"messaging.system": "rabbitmq"})
		t.span("pw", "op", "payment-worker", "", "consume order.created", core.KindConsumer, pub+3_000, jitter(9_000), false, nil)
	}
	cur += ord + 1_000
	t.events[0].Duration = cur
}

func statusCode(failed bool) float64 {
	if failed {
		return 503
	}
	return 201
}

func browse(t *tracer) {
	hit := rand.Float64() < 0.7
	t.span("g", "", "gateway", "", "GET /api/products/:id", core.KindServer, 0, 0, false, core.Attributes{"http.method": "GET", "http.route": "/api/products/:id"})
	t.span("gc", "g", "gateway", "catalog", "GET /products/:id", core.KindClient, 800, 0, false, nil)
	cat := jitter(1_800)
	t.span("c", "gc", "catalog", "", "GET /products/:id", core.KindServer, 1_200, 0, false, nil)
	t.span("cr", "c", "catalog", "redis", "GET product", core.KindClient, 1_500, cat/2, false, core.Attributes{"db.system": "redis", "cache.hit": hit})
	end := 1_500 + cat/2
	if !hit {
		pg := jitter(6_000)
		t.span("cq", "c", "catalog", "postgresql", "SELECT product", core.KindClient, end+300, pg, false, core.Attributes{"db.system": "postgresql"})
		t.span("cw", "c", "catalog", "redis", "SET product", core.KindClient, end+pg+600, 400, false, core.Attributes{"db.system": "redis"})
		end += pg + 1_200
	}
	t.events[2].Duration = end - 1_200 + 300 // catalog server span
	t.events[1].Duration = end - 800 + 600   // gateway client span
	t.events[0].Duration = end + 1_200
}

func login(t *tracer, errRate float64) {
	bad := rand.Float64() < errRate/3 // wrong passwords are a client error, not an outage
	d := jitter(12_000)
	t.span("g", "", "gateway", "", "POST /api/login", core.KindServer, 0, d+3_000, false, core.Attributes{"http.method": "POST", "http.route": "/api/login"})
	t.span("ga", "g", "gateway", "auth", "POST /login", core.KindClient, 1_000, d+1_000, false, nil)
	t.span("a", "ga", "auth", "", "POST /login", core.KindServer, 1_500, d, false, core.Attributes{"http.status_code": map[bool]float64{true: 401, false: 200}[bad]})
	t.span("aq", "a", "auth", "postgresql", "SELECT user", core.KindClient, 2_000, d/4, false, core.Attributes{"db.system": "postgresql"})
	t.span("ah", "a", "auth", "", "bcrypt.compare", core.KindInternal, 2_000+d/4+200, d/2, false, nil)
}

func main() {
	log.SetFlags(0)
	base := strings.TrimRight(os.Getenv("LIVEPROBE_URL"), "/")
	if base == "" {
		base = "http://localhost:4319"
	}
	rate := envFloat("TRACES_PER_SEC", 6)
	errRate := envFloat("ERROR_RATE", 0.12)
	duration := time.Duration(envFloat("DURATION_SECONDS", 0) * float64(time.Second))
	if rate <= 0 {
		log.Fatal("[loadgen] TRACES_PER_SEC must be > 0")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	var deadline <-chan time.Time
	if duration > 0 {
		deadline = time.After(duration)
	}
	tick := time.NewTicker(time.Duration(float64(time.Second) / rate))
	defer tick.Stop()
	client := &http.Client{Timeout: 5 * time.Second}
	log.Printf("[loadgen] %.1f traces/s, error rate %.0f%% -> %s/v1/events", rate, errRate*100, base)

	sent, failures := 0, 0
	for n := 0; ; n++ {
		select {
		case <-stop:
			log.Printf("[loadgen] stopped after %d traces", sent)
			return
		case <-deadline:
			log.Printf("[loadgen] done: %d traces", sent)
			return
		case <-tick.C:
		}
		t := &tracer{id: fmt.Sprintf("lg-%d-%d", time.Now().UnixMilli(), n), start: time.Now().UnixMicro()}
		switch r := rand.Float64(); {
		case r < 0.45:
			checkout(t, errRate)
		case r < 0.85:
			browse(t)
		default:
			login(t, errRate)
		}
		body, _ := json.Marshal(map[string]any{"events": t.events})
		res, err := client.Post(base+"/v1/events", "application/json", bytes.NewReader(body))
		if err != nil {
			if failures++; failures%20 == 1 {
				log.Printf("[loadgen] post failed (is the server up?): %v", err)
			}
			continue
		}
		res.Body.Close()
		sent++
	}
}
