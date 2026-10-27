package collector

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// The minimal slice of AMQP the source uses. Keeping our own interfaces (rather than amqp091's
// concrete types) lets tests drive a fake broker, including dropped connections.

type Delivery interface {
	Body() []byte
	Ack() error
	Nack(requeue bool) error
}

type Channel interface {
	ExchangeDeclare(name, kind string, durable bool) error
	ExchangeDeclarePassive(name string) error
	QueueDeclare(name string, durable bool, args map[string]any) error
	QueueBind(queue, exchange, key string) error
	Qos(prefetch int) error
	Consume(queue string) (<-chan Delivery, error)
	Close() error
}

type Conn interface {
	Channel() (Channel, error)
	// NotifyClose yields (or closes) when the connection is lost or closed.
	NotifyClose() <-chan error
	Close() error
}

type Dialer func(url string) (Conn, error)

type RabbitMQOptions struct {
	// Tap an existing exchange: bind our own queue to it so we get a *copy* of the stream without
	// competing with the client's consumers. Empty = consume the queue directly.
	Exchange string
	// If set, declare the exchange with this type (durable). If empty, only verify it exists
	// (passive) — the safe default when tapping an exchange someone else owns.
	ExchangeType string
	// Binding keys; default ["#"].
	RoutingKeys []string
	// Queue durability; nil = durable.
	QueueDurable *bool
	// Bound a tap queue so it can't grow unbounded while the collector is down (0 = unset).
	QueueMaxLength    int // x-max-length with drop-head: keep the newest events for a live view
	QueueMessageTTLMs int // x-message-ttl
	// Base reconnect delay; doubles per failed attempt up to 30s. Default 1s.
	ReconnectDelay time.Duration
	// Injectable connector for tests. Default dials amqp091.
	Dial Dialer
}

// RabbitMQSource consumes a queue of raw source events. A message is acked after the handler
// runs; invalid JSON (or a handler panic) is nacked without requeue so a poison message can't
// wedge the queue. A lost connection reconnects with exponential backoff and re-runs the full
// setup. Only the initial connect error is returned — fail fast on misconfiguration.
func RabbitMQSource(url, queue string, onEvent func(any), o RabbitMQOptions) (stop func(), err error) {
	if o.Dial == nil {
		o.Dial = dialAMQP
	}
	if o.ReconnectDelay <= 0 {
		o.ReconnectDelay = time.Second
	}
	r := &rabbit{url: url, queue: queue, onEvent: onEvent, o: o, quit: make(chan struct{})}
	conn, ch, err := r.setup()
	if err != nil {
		return nil, err
	}
	r.wg.Add(1)
	go r.supervise(conn, ch)
	return r.stop, nil
}

type rabbit struct {
	url, queue string
	onEvent    func(any)
	o          RabbitMQOptions

	mu   sync.Mutex
	conn Conn
	ch   Channel

	quit     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func (r *rabbit) setup() (Conn, Channel, error) {
	conn, err := r.o.Dial(r.url)
	if err != nil {
		return nil, nil, err
	}
	ch, err := r.configure(conn)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	deliveries, err := ch.Consume(r.queue)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	r.mu.Lock()
	r.conn, r.ch = conn, ch
	r.mu.Unlock()
	go r.consume(ch, deliveries)
	return conn, ch, nil
}

func (r *rabbit) configure(conn Conn) (Channel, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	var args map[string]any
	if r.o.QueueMaxLength > 0 || r.o.QueueMessageTTLMs > 0 {
		args = map[string]any{}
		if r.o.QueueMaxLength > 0 {
			args["x-max-length"] = int64(r.o.QueueMaxLength)
			args["x-overflow"] = "drop-head"
		}
		if r.o.QueueMessageTTLMs > 0 {
			args["x-message-ttl"] = int64(r.o.QueueMessageTTLMs)
		}
	}
	durable := r.o.QueueDurable == nil || *r.o.QueueDurable

	if r.o.Exchange != "" {
		if r.o.ExchangeType != "" {
			err = ch.ExchangeDeclare(r.o.Exchange, r.o.ExchangeType, true)
		} else {
			err = ch.ExchangeDeclarePassive(r.o.Exchange)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := ch.QueueDeclare(r.queue, durable, args); err != nil {
		return nil, err
	}
	if r.o.Exchange != "" {
		keys := r.o.RoutingKeys
		if len(keys) == 0 {
			keys = []string{"#"}
		}
		for _, k := range keys {
			if err := ch.QueueBind(r.queue, r.o.Exchange, k); err != nil {
				return nil, err
			}
		}
	}
	return ch, ch.Qos(64)
}

func (r *rabbit) consume(_ Channel, deliveries <-chan Delivery) {
	for d := range deliveries {
		if r.handle(d.Body()) {
			_ = d.Ack()
		} else {
			_ = d.Nack(false)
		}
	}
}

// handle reports whether the message was processed; invalid JSON and handler panics are not.
func (r *rabbit) handle(body []byte) (ok bool) {
	defer func() {
		if p := recover(); p != nil {
			ok = false
		}
	}()
	var v any
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	r.onEvent(v)
	return true
}

// supervise waits for the connection to drop and reconnects until stop.
func (r *rabbit) supervise(conn Conn, _ Channel) {
	defer r.wg.Done()
	for {
		select {
		case <-r.quit:
			return
		case <-conn.NotifyClose():
		}
		r.mu.Lock()
		r.conn, r.ch = nil, nil
		r.mu.Unlock()
		delay := r.o.ReconnectDelay
		for {
			select {
			case <-r.quit:
				return
			default:
			}
			log.Printf("[collector] rabbitmq connection lost; reconnecting in %v", delay)
			select {
			case <-r.quit:
				return
			case <-time.After(delay):
			}
			c, _, err := r.setup()
			if err == nil {
				conn = c
				break
			}
			delay = min(delay*2, 30*time.Second)
		}
	}
}

func (r *rabbit) stop() {
	r.stopOnce.Do(func() {
		close(r.quit)
		r.mu.Lock()
		conn, ch := r.conn, r.ch
		r.conn, r.ch = nil, nil
		r.mu.Unlock()
		if ch != nil {
			_ = ch.Close()
		}
		if conn != nil {
			_ = conn.Close()
		}
		r.wg.Wait()
	})
}

// --- amqp091 adapter -----------------------------------------------------------------------

type amqpConn struct {
	c      *amqp.Connection
	closed chan error
}

func dialAMQP(url string) (Conn, error) {
	c, err := amqp.Dial(url)
	if err != nil {
		return nil, err
	}
	notify := c.NotifyClose(make(chan *amqp.Error, 1))
	closed := make(chan error, 1)
	go func() {
		if e, ok := <-notify; ok && e != nil {
			closed <- e
		}
		close(closed)
	}()
	return &amqpConn{c: c, closed: closed}, nil
}

func (a *amqpConn) NotifyClose() <-chan error { return a.closed }
func (a *amqpConn) Close() error              { return a.c.Close() }
func (a *amqpConn) Channel() (Channel, error) {
	ch, err := a.c.Channel()
	if err != nil {
		return nil, err
	}
	return &amqpChannel{ch}, nil
}

type amqpChannel struct{ ch *amqp.Channel }

func (a *amqpChannel) ExchangeDeclare(name, kind string, durable bool) error {
	return a.ch.ExchangeDeclare(name, kind, durable, false, false, false, nil)
}

func (a *amqpChannel) ExchangeDeclarePassive(name string) error {
	// A passive declare only checks existence; the broker ignores the other fields.
	return a.ch.ExchangeDeclarePassive(name, "topic", true, false, false, false, nil)
}

func (a *amqpChannel) QueueDeclare(name string, durable bool, args map[string]any) error {
	_, err := a.ch.QueueDeclare(name, durable, false, false, false, amqp.Table(args))
	return err
}

func (a *amqpChannel) QueueBind(queue, exchange, key string) error {
	return a.ch.QueueBind(queue, key, exchange, false, nil)
}

func (a *amqpChannel) Qos(n int) error { return a.ch.Qos(n, 0, false) }
func (a *amqpChannel) Close() error    { return a.ch.Close() }

func (a *amqpChannel) Consume(queue string) (<-chan Delivery, error) {
	msgs, err := a.ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return nil, fmt.Errorf("consume %s: %w", queue, err)
	}
	out := make(chan Delivery)
	go func() {
		defer close(out)
		for m := range msgs {
			out <- amqpDelivery{m}
		}
	}()
	return out, nil
}

type amqpDelivery struct{ d amqp.Delivery }

func (a amqpDelivery) Body() []byte            { return a.d.Body }
func (a amqpDelivery) Ack() error              { return a.d.Ack(false) }
func (a amqpDelivery) Nack(requeue bool) error { return a.d.Nack(false, requeue) }
