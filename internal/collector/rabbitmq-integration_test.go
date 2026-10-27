package collector

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Exercises the real amqp091 path against a live broker. Opt-in:
//
//	LIVEPROBE_AMQP_URL=amqp://guest:guest@localhost:5699/ \
//	LIVEPROBE_AMQP_KILL="docker exec lp-go-rabbit-test rabbitmqctl close_all_connections test" \
//	go test -run Integration ./internal/collector/
//
// LIVEPROBE_AMQP_KILL (optional) force-closes connections broker-side to prove reconnection.
func TestRabbitMQIntegration(t *testing.T) {
	url := os.Getenv("LIVEPROBE_AMQP_URL")
	if url == "" {
		t.Skip("LIVEPROBE_AMQP_URL not set")
	}
	exchange, queue := "lp.it.events", "lp.it.tap"

	rec := &recorder{}
	stop, err := RabbitMQSource(url, queue, rec.on, RabbitMQOptions{
		Exchange: exchange, ExchangeType: "topic", RoutingKeys: []string{"orders.#"},
		QueueMaxLength: 1000, QueueMessageTTLMs: 60_000, ReconnectDelay: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pubConn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	defer pubConn.Close()
	pub, err := pubConn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	publish := func(key, body string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := pub.PublishWithContext(ctx, exchange, key, false, false, amqp.Publishing{ContentType: "application/json", Body: []byte(body)}); err != nil {
			t.Fatal(err)
		}
	}
	waitCount := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for len(rec.got()) < n {
			if time.Now().After(deadline) {
				t.Fatalf("received %d messages, want %d", len(rec.got()), n)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	publish("orders.created", `{"trace_id":"t1","span_id":"s1"}`)
	publish("orders.created", "{poison")                       // nacked without requeue, stream continues
	publish("payments.done", `{"trace_id":"x","span_id":"y"}`) // not bound → never delivered
	publish("orders.paid", `{"trace_id":"t2","span_id":"s2"}`)
	waitCount(2)

	kill := os.Getenv("LIVEPROBE_AMQP_KILL")
	if kill == "" {
		return
	}
	// The kill also drops the publisher; reconnect it after the consumer is back.
	parts := strings.Fields(kill)
	if out, err := exec.Command(parts[0], parts[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("kill: %v\n%s", err, out)
	}
	time.Sleep(1500 * time.Millisecond) // reconnect delay + full setup
	pubConn2, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	defer pubConn2.Close()
	if pub, err = pubConn2.Channel(); err != nil {
		t.Fatal(err)
	}
	publish("orders.after-reconnect", `{"trace_id":"t3","span_id":"s3"}`)
	waitCount(3)
	if got := len(rec.got()); got != 3 {
		t.Errorf("received %d, want exactly 3 (no poison, no unbound key)", got)
	}
}
