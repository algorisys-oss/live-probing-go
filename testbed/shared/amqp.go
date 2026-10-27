package shared

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RabbitMQ with trace context carried across the broker. There is no maintained Go AMQP
// instrumentation, so this does by hand what Node's amqplib instrumentation did: a producer span
// per publish with the traceparent injected into the message headers, and a consumer span per
// delivery parented on the extracted context — so a checkout stays one trace from the gateway,
// through order, to the payment / inventory / notification workers.

const tracerName = "shopwave/amqp"

// headerCarrier adapts amqp.Table to OTel's TextMapCarrier.
type headerCarrier amqp.Table

func (h headerCarrier) Get(key string) string {
	if v, ok := h[key].(string); ok {
		return v
	}
	return ""
}
func (h headerCarrier) Set(key, value string) { h[key] = value }
func (h headerCarrier) Keys() []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	return keys
}

func messagingAttrs(routingKey string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("messaging.system", "rabbitmq"),
		attribute.String("messaging.destination.name", OrderExchange),
		attribute.String("messaging.rabbitmq.destination.routing_key", routingKey),
	}
}

type Rabbit struct {
	conn *amqp.Connection
	mu   sync.Mutex // guards pub
	pub  *amqp.Channel
}

// ConnectRabbit connects and asserts the shared topic exchange. It retries for up to ~2 minutes:
// RabbitMQ's healthcheck (rabbitmq-diagnostics ping) passes before the AMQP listener accepts
// connections, so a dependent service can start into that gap.
func ConnectRabbit() *Rabbit {
	url := Env("RABBITMQ_URL", "amqp://localhost:5672")
	var conn *amqp.Connection
	var err error
	deadline := time.Now().Add(2 * time.Minute)
	for delay := 500 * time.Millisecond; ; delay = min(delay*2, 5*time.Second) {
		if conn, err = amqp.Dial(url); err == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Fatalf("rabbitmq not reachable: %v", err)
		}
		time.Sleep(delay)
	}
	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("rabbitmq channel: %v", err)
	}
	if err := ch.ExchangeDeclare(OrderExchange, "topic", true, false, false, false, nil); err != nil {
		log.Fatalf("rabbitmq exchange: %v", err)
	}
	return &Rabbit{conn: conn, pub: ch}
}

// Publish sends a JSON message as a producer span, carrying the trace context in the headers.
func (r *Rabbit) Publish(ctx context.Context, routingKey string, message any) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "publish "+routingKey,
		trace.WithSpanKind(trace.SpanKindProducer), trace.WithAttributes(messagingAttrs(routingKey)...))
	defer span.End()
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	headers := amqp.Table{}
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier(headers))
	r.mu.Lock()
	err = r.pub.PublishWithContext(ctx, OrderExchange, routingKey, false, false, amqp.Publishing{
		ContentType: "application/json", DeliveryMode: amqp.Persistent, Headers: headers, Body: body,
	})
	r.mu.Unlock()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// MessageHandler processes one message; an error nacks it without requeue (a poison message
// can't wedge the queue).
type MessageHandler func(ctx context.Context, body []byte, routingKey string) error

// Consume asserts a durable queue bound to the exchange for each routing key and processes
// deliveries concurrently (bounded by prefetch 16), each as a consumer span in the producer's trace.
func (r *Rabbit) Consume(queue string, routingKeys []string, handle MessageHandler) {
	ch, err := r.conn.Channel()
	if err != nil {
		log.Fatalf("rabbitmq channel: %v", err)
	}
	if err := ch.Qos(16, 0, false); err != nil {
		log.Fatalf("rabbitmq qos: %v", err)
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		log.Fatalf("rabbitmq queue %s: %v", queue, err)
	}
	for _, rk := range routingKeys {
		if err := ch.QueueBind(queue, rk, OrderExchange, false, nil); err != nil {
			log.Fatalf("rabbitmq bind %s: %v", queue, err)
		}
	}
	deliveries, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("rabbitmq consume %s: %v", queue, err)
	}
	go func() {
		for d := range deliveries {
			go r.process(d, handle)
		}
	}()
}

func (r *Rabbit) process(d amqp.Delivery, handle MessageHandler) {
	headers := d.Headers
	if headers == nil {
		headers = amqp.Table{}
	}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), headerCarrier(headers))
	ctx, span := otel.Tracer(tracerName).Start(ctx, "process "+d.RoutingKey,
		trace.WithSpanKind(trace.SpanKindConsumer), trace.WithAttributes(messagingAttrs(d.RoutingKey)...))
	defer span.End()
	if err := handle(ctx, d.Body, d.RoutingKey); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		_ = d.Nack(false, false)
		return
	}
	_ = d.Ack(false)
}

func (r *Rabbit) Close() {
	_ = r.pub.Close()
	_ = r.conn.Close()
}
