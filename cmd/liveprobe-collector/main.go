// Command liveprobe-collector bridges a foreign telemetry stream into LiveProbe: one source
// (transport) + one adapter (format) → POST /v1/events. Configure per client with env vars; run
// one per client (sidecar) or several instances.
//
//	SOURCE=stdin    ADAPTER=adapter-id-1                      < events.ndjson
//	SOURCE=rabbitmq ADAPTER=adapter-id-1 RABBITMQ_URL=... QUEUE=instrumentation.events
//	SOURCE=http     ADAPTER=algo-instrumentation HTTP_PORT=4320   (accepts POSTed JSON events)
//
// To tap an existing exchange non-intrusively (bind our own queue for a copy of the stream):
//
//	SOURCE=rabbitmq ADAPTER=adapter-id-1 RABBITMQ_URL=... QUEUE=liveprobe.tap \
//	  EXCHANGE=instrumentation ROUTING_KEY="#"   [EXCHANGE_TYPE=topic to declare it]
//
// Optionally bound the tap queue so it can't grow unbounded while the collector is down:
//
//	QUEUE_MAX_LENGTH=100000 (drops oldest past the cap)   QUEUE_MESSAGE_TTL_MS=60000
//
// LIVEPROBE_URL defaults to http://localhost:4319.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"liveprobe/internal/collector"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func posInt(name string) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func main() {
	log.SetFlags(0)
	source := env("SOURCE", "stdin")
	adapterName := env("ADAPTER", "adapter-id-1")
	liveprobeURL := env("LIVEPROBE_URL", "http://localhost:4319")

	adapt, err := collector.GetAdapter(adapterName)
	if err != nil {
		log.Fatalf("[collector] %v", err)
	}
	sink := collector.NewSink(liveprobeURL, 50, time.Second)
	onEvent := func(raw any) {
		defer func() { _ = recover() }() // one bad event never stops the stream
		sink.Push(adapt(raw))
	}
	log.Printf("[collector] source=%s adapter=%s -> %s/v1/events", source, adapterName, strings.TrimRight(liveprobeURL, "/"))

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	switch source {
	case "stdin":
		collector.ReadNDJSON(os.Stdin, onEvent)
		sink.Close()

	case "http":
		port := env("HTTP_PORT", "4320")
		srv := &http.Server{Addr: net.JoinHostPort("", port), Handler: collector.HTTPSourceHandler(onEvent), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-stop
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
		log.Printf("[collector] http source listening on :%s (POST JSON events, any path)", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[collector] %v", err)
		}
		sink.Close()

	case "rabbitmq":
		var keys []string
		for _, k := range strings.Split(env("ROUTING_KEY", "#"), ",") {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
		o := collector.RabbitMQOptions{
			Exchange:          os.Getenv("EXCHANGE"),
			ExchangeType:      os.Getenv("EXCHANGE_TYPE"),
			RoutingKeys:       keys,
			QueueMaxLength:    posInt("QUEUE_MAX_LENGTH"),
			QueueMessageTTLMs: posInt("QUEUE_MESSAGE_TTL_MS"),
		}
		queue := env("QUEUE", "instrumentation.events")
		if o.Exchange != "" {
			mode := " (verify)"
			if o.ExchangeType != "" {
				mode = " (declare " + o.ExchangeType + ")"
			}
			log.Printf("[collector] tapping exchange=%q%s keys=[%s] -> queue=%q", o.Exchange, mode, strings.Join(keys, ", "), queue)
		}
		stopSource, err := collector.RabbitMQSource(env("RABBITMQ_URL", "amqp://localhost:5672"), queue, onEvent, o)
		if err != nil {
			log.Fatalf("[collector] fatal: %v", err)
		}
		<-stop
		stopSource()
		sink.Close()

	default:
		log.Fatalf("[collector] unknown SOURCE %q (use stdin, rabbitmq, or http)", source)
	}
}
