package collector

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"liveprobe/internal/core"
)

// Ports of sources/http.test.ts and sources/rabbitmq.test.ts, plus stdin and sink tests (the TS
// collector had none for those).

type recorder struct {
	mu   sync.Mutex
	seen []any
}

func (r *recorder) on(raw any) {
	r.mu.Lock()
	r.seen = append(r.seen, raw)
	r.mu.Unlock()
}

func (r *recorder) got() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.seen...)
}

func send(t *testing.T, method, url, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestHTTPSourceAcceptsEventOnAnyPath(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(HTTPSourceHandler(rec.on))
	defer srv.Close()
	eq(t, send(t, "POST", srv.URL+"/v1/event/instrumentation", `{"eventId":"e1","eventType":"instrumentation"}`), 202, "status")
	if !reflect.DeepEqual(rec.got(), []any{map[string]any{"eventId": "e1", "eventType": "instrumentation"}}) {
		t.Errorf("received: %v", rec.got())
	}
}

func TestHTTPSourceFansOutArray(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(HTTPSourceHandler(rec.on))
	defer srv.Close()
	eq(t, send(t, "POST", srv.URL+"/v1/event/log", `[{"eventId":"a"},{"eventId":"b"}]`), 202, "status")
	eq(t, len(rec.got()), 2, "received")
}

func TestHTTPSourceRejectsInvalidJSON(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(HTTPSourceHandler(rec.on))
	defer srv.Close()
	eq(t, send(t, "POST", srv.URL+"/v1/event/audit", "{not json"), 400, "status")
	eq(t, len(rec.got()), 0, "dropped")
}

func TestHTTPSourceHealthAndMethods(t *testing.T) {
	srv := httptest.NewServer(HTTPSourceHandler(func(any) {}))
	defer srv.Close()
	eq(t, send(t, "GET", srv.URL+"/healthz", ""), 200, "healthz")
	eq(t, send(t, "GET", srv.URL+"/v1/event/log", ""), 405, "GET")
	eq(t, send(t, "POST", srv.URL+"/x", `"`+strings.Repeat("a", 6*1024*1024)+`"`), 413, "too large")
}

func TestStdinSourceSkipsBlankAndMalformedLines(t *testing.T) {
	rec := &recorder{}
	ReadNDJSON(strings.NewReader("{\"a\":1}\n\n  \nnot json\n[1,2]\n"), rec.on)
	if !reflect.DeepEqual(rec.got(), []any{map[string]any{"a": float64(1)}, []any{float64(1), float64(2)}}) {
		t.Errorf("received: %v", rec.got())
	}
}

func TestSinkBatchesAndFlushes(t *testing.T) {
	var mu sync.Mutex
	var batches [][]core.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			t.Errorf("path %s", r.URL.Path)
		}
		var body struct{ Events []core.Event }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		batches = append(batches, body.Events)
		mu.Unlock()
	}))
	defer srv.Close()

	s := NewSink(srv.URL+"/", 3, time.Hour) // trailing slash is trimmed
	ev := func(id string) core.Event { return core.Event{TraceID: "t", SpanID: id, Attributes: core.Attributes{}} }
	s.Push(nil)
	s.Push([]core.Event{ev("a"), ev("b"), ev("c")}) // hits the batch size → async flush
	s.Push([]core.Event{ev("d")})
	s.Close() // flushes the remainder and waits for in-flight posts

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for _, b := range batches {
		total += len(b)
	}
	eq(t, len(batches), 2, "batches")
	eq(t, total, 4, "events delivered")
}

// --- rabbitmq ------------------------------------------------------------------------------

// brokerState is what the fake records; snapshot() copies it out from under the lock.
type brokerState struct {
	connects      int
	assertExch    [][]any
	checkExch     []string
	assertQueue   [][]any
	binds         [][3]string
	prefetch      []int
	acked, nacked int
	channelClosed bool
	connClosed    bool
}

type fakeBroker struct {
	mu sync.Mutex
	brokerState
	failConnects map[int]bool
	conns        []*fakeConn
	deliveries   chan Delivery
}

type fakeConn struct {
	b      *fakeBroker
	closed chan error
	once   sync.Once
}

func (c *fakeConn) Channel() (Channel, error) { return &fakeChannel{c.b}, nil }
func (c *fakeConn) NotifyClose() <-chan error { return c.closed }
func (c *fakeConn) drop()                     { c.once.Do(func() { c.closed <- errors.New("dropped"); close(c.closed) }) }
func (c *fakeConn) Close() error {
	c.b.mu.Lock()
	c.b.connClosed = true
	c.b.mu.Unlock()
	c.once.Do(func() { close(c.closed) }) // a graceful close also notifies
	return nil
}

type fakeChannel struct{ b *fakeBroker }

func (f *fakeChannel) lock() func() { f.b.mu.Lock(); return f.b.mu.Unlock }
func (f *fakeChannel) ExchangeDeclare(name, kind string, durable bool) error {
	defer f.lock()()
	f.b.assertExch = append(f.b.assertExch, []any{name, kind, durable})
	return nil
}
func (f *fakeChannel) ExchangeDeclarePassive(name string) error {
	defer f.lock()()
	f.b.checkExch = append(f.b.checkExch, name)
	return nil
}
func (f *fakeChannel) QueueDeclare(name string, durable bool, args map[string]any) error {
	defer f.lock()()
	f.b.assertQueue = append(f.b.assertQueue, []any{name, durable, args})
	return nil
}
func (f *fakeChannel) QueueBind(queue, exchange, key string) error {
	defer f.lock()()
	f.b.binds = append(f.b.binds, [3]string{queue, exchange, key})
	return nil
}
func (f *fakeChannel) Qos(n int) error {
	defer f.lock()()
	f.b.prefetch = append(f.b.prefetch, n)
	return nil
}
func (f *fakeChannel) Consume(string) (<-chan Delivery, error) {
	defer f.lock()()
	f.b.deliveries = make(chan Delivery, 16)
	return f.b.deliveries, nil
}
func (f *fakeChannel) Close() error {
	defer f.lock()()
	f.b.channelClosed = true
	return nil
}

type fakeDelivery struct {
	b    *fakeBroker
	body []byte
	done chan struct{}
}

func (d *fakeDelivery) Body() []byte { return d.body }
func (d *fakeDelivery) Ack() error {
	d.b.mu.Lock()
	d.b.acked++
	d.b.mu.Unlock()
	close(d.done)
	return nil
}
func (d *fakeDelivery) Nack(requeue bool) error {
	if requeue {
		panic("poison messages must not be requeued")
	}
	d.b.mu.Lock()
	d.b.nacked++
	d.b.mu.Unlock()
	close(d.done)
	return nil
}

func (b *fakeBroker) dial(string) (Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.connects++
	if b.failConnects[b.connects] {
		return nil, errors.New("connect refused")
	}
	c := &fakeConn{b: b, closed: make(chan error, 1)}
	b.conns = append(b.conns, c)
	return c, nil
}

// deliver pushes a message and waits until it was acked or nacked.
func (b *fakeBroker) deliver(t *testing.T, body string) {
	t.Helper()
	b.mu.Lock()
	ch := b.deliveries
	b.mu.Unlock()
	d := &fakeDelivery{b: b, body: []byte(body), done: make(chan struct{})}
	ch <- d
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery not settled")
	}
}

func (b *fakeBroker) snapshot() brokerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.brokerState
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestRabbitPlainQueueConsume(t *testing.T) {
	b := &fakeBroker{}
	rec := &recorder{}
	stop, err := RabbitMQSource("amqp://x", "q1", rec.on, RabbitMQOptions{Dial: b.dial})
	if err != nil {
		t.Fatal(err)
	}
	s := b.snapshot()
	if !reflect.DeepEqual(s.assertQueue[0], []any{"q1", true, map[string]any(nil)}) || len(s.assertExch) != 0 || len(s.binds) != 0 || !reflect.DeepEqual(s.prefetch, []int{64}) {
		t.Errorf("setup: %+v", s)
	}
	b.deliver(t, `{"trace_id":"t","span_id":"s"}`)
	eq(t, len(rec.got()), 1, "consumed")
	eq(t, b.snapshot().acked, 1, "acked")
	stop()
	s = b.snapshot()
	if !s.channelClosed || !s.connClosed {
		t.Error("stop closes channel and connection")
	}
}

func TestRabbitExchangeTapPassive(t *testing.T) {
	b := &fakeBroker{}
	stop, _ := RabbitMQSource("amqp://x", "liveprobe.tap", func(any) {}, RabbitMQOptions{Dial: b.dial, Exchange: "instrumentation", RoutingKeys: []string{"orders.#", "payments.#"}})
	defer stop()
	s := b.snapshot()
	if !reflect.DeepEqual(s.checkExch, []string{"instrumentation"}) || len(s.assertExch) != 0 {
		t.Errorf("exchange: %+v", s)
	}
	if !reflect.DeepEqual(s.binds, [][3]string{{"liveprobe.tap", "instrumentation", "orders.#"}, {"liveprobe.tap", "instrumentation", "payments.#"}}) {
		t.Errorf("binds: %v", s.binds)
	}
}

func TestRabbitExchangeTapDeclares(t *testing.T) {
	b := &fakeBroker{}
	stop, _ := RabbitMQSource("amqp://x", "tap", func(any) {}, RabbitMQOptions{Dial: b.dial, Exchange: "events", ExchangeType: "topic"})
	defer stop()
	s := b.snapshot()
	if !reflect.DeepEqual(s.assertExch[0], []any{"events", "topic", true}) || len(s.checkExch) != 0 || !reflect.DeepEqual(s.binds, [][3]string{{"tap", "events", "#"}}) {
		t.Errorf("declare: %+v", s)
	}
}

func TestRabbitBoundedQueue(t *testing.T) {
	b := &fakeBroker{}
	stop, _ := RabbitMQSource("amqp://x", "liveprobe.tap", func(any) {}, RabbitMQOptions{Dial: b.dial, Exchange: "instrumentation", QueueMaxLength: 100_000, QueueMessageTTLMs: 60_000})
	defer stop()
	want := map[string]any{"x-max-length": int64(100_000), "x-overflow": "drop-head", "x-message-ttl": int64(60_000)}
	if !reflect.DeepEqual(b.snapshot().assertQueue[0][2], want) {
		t.Errorf("args: %v", b.snapshot().assertQueue[0][2])
	}
}

func TestRabbitReconnectsAfterDrop(t *testing.T) {
	b := &fakeBroker{}
	rec := &recorder{}
	stop, _ := RabbitMQSource("amqp://x", "q", rec.on, RabbitMQOptions{Dial: b.dial, ReconnectDelay: time.Millisecond})
	defer stop()
	b.conns[0].drop() // broker restart / heartbeat timeout
	waitFor(t, func() bool { return len(b.snapshot().assertQueue) == 2 })
	eq(t, b.snapshot().connects, 2, "reconnected")
	b.deliver(t, `{"trace_id":"t","span_id":"s"}`) // consumer works again
	eq(t, len(rec.got()), 1, "consumed after reconnect")
}

func TestRabbitRetriesFailedReconnect(t *testing.T) {
	b := &fakeBroker{failConnects: map[int]bool{2: true}}
	stop, _ := RabbitMQSource("amqp://x", "q", func(any) {}, RabbitMQOptions{Dial: b.dial, ReconnectDelay: time.Millisecond})
	defer stop()
	b.conns[0].drop()
	waitFor(t, func() bool { return b.snapshot().connects == 3 }) // initial + failed + successful
}

func TestRabbitStopPreventsReconnect(t *testing.T) {
	b := &fakeBroker{}
	stop, _ := RabbitMQSource("amqp://x", "q", func(any) {}, RabbitMQOptions{Dial: b.dial, ReconnectDelay: time.Millisecond})
	stop()
	time.Sleep(25 * time.Millisecond)
	eq(t, b.snapshot().connects, 1, "no reconnect after stop")
}

func TestRabbitInitialConnectFailureIsReturned(t *testing.T) {
	b := &fakeBroker{failConnects: map[int]bool{1: true}}
	if _, err := RabbitMQSource("amqp://x", "q", func(any) {}, RabbitMQOptions{Dial: b.dial}); err == nil {
		t.Error("initial connect must fail fast")
	}
}

func TestRabbitPoisonMessageNacked(t *testing.T) {
	b := &fakeBroker{}
	rec := &recorder{}
	stop, _ := RabbitMQSource("amqp://x", "q", rec.on, RabbitMQOptions{Dial: b.dial})
	defer stop()
	b.deliver(t, "{not json")
	eq(t, len(rec.got()), 0, "not delivered")
	eq(t, b.snapshot().nacked, 1, "nacked")
	b.deliver(t, `{"ok":true}`) // stream still works
	eq(t, len(rec.got()), 1, "later message delivered")
	eq(t, b.snapshot().acked, 1, "acked")
}

func TestRabbitPanickingHandlerNacks(t *testing.T) {
	b := &fakeBroker{}
	stop, _ := RabbitMQSource("amqp://x", "q", func(any) { panic("adapter bug") }, RabbitMQOptions{Dial: b.dial})
	defer stop()
	b.deliver(t, `{}`)
	eq(t, b.snapshot().nacked, 1, "handler failure nacks, does not crash")
}
