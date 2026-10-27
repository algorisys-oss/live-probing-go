# Shopwave testbed (Go)

A small but real e-commerce system that produces honest traces for LiveProbe: eight Go services,
Postgres, Redis, RabbitMQ, OpenTelemetry → collector → Jaeger + LiveProbe. It is the Go port of
`../../live-probing/testbed` with the same topology, ports, routes, data, events and failure scenario.

```
loadgen ─► gateway ─┬─► auth ──────────► postgres, redis (sessions)
                    ├─► catalog ───────► postgres, redis (read-through cache)
                    ├─► cart ──┬───────► redis, catalog (one call per line: an N+1)
                    │          └─► order ► postgres ─► RabbitMQ order.created
                    └─► order                              ├─► payment-worker   ► postgres ─► payment.*
                                                           ├─► inventory-worker ► postgres ─► inventory.*
                         order saga ◄── payment.* / inventory.* ──► notification-worker ► redis
```

## Run it

From the repo root, `./dev-start.sh --testbed` builds the image, starts everything, starts the
loadgen, and runs LiveProbe on :4319 (`--chaos` adds the failure scenario; `./dev-stop.sh` stops it
all). Directly:

```bash
docker compose up -d --build                     # services + infra
docker compose --profile load up -d loadgen      # traffic (waits until every service is healthy)
docker compose -f docker-compose.yml -f docker-compose.chaos.yml --profile load up -d   # + failures
docker compose --profile load down               # stop (-v also drops the Postgres volume)
```

| Endpoint | URL |
|---|---|
| Gateway API | http://localhost:3000 (`/api/products`, `/api/login`, …) |
| Jaeger | http://localhost:16687 |
| Postgres / Redis / RabbitMQ | `localhost:55432` / `56379` / `55672` (management UI :15673) |

Sample users (password `password123`): `alice@shopwave.test`, `bob@shopwave.test`, `carol@shopwave.test`.

## Layout

```
cmd/<service>/   gateway, auth, catalog, cart, order, payment-worker, inventory-worker,
                 notification-worker, loadgen, healthcheck (the distroless healthcheck probe)
shared/          env, fault injection (chaos.go), client resilience (resilient.go), contracts,
                 and the instrumented plumbing: otel.go, http.go, stores.go (pgx, go-redis), amqp.go
Dockerfile       one distroless image; each compose service runs a different binary
```

It is its own Go module (`shopwave`), so the OpenTelemetry SDK, pgx, go-redis and x/crypto never
become dependencies of LiveProbe itself.

## How it is instrumented

Go has no auto-instrumentation, so tracing is explicit — and shaped to read like the Node testbed:

- **HTTP** — `otelhttp` on every server (span named by route pattern, `http.route` stamped after
  routing) and client (`METHOD /path`), with W3C `traceparent` propagation.
- **Postgres** — `otelpgx`, spans named `pg.query:<VERB>` like the Node pg instrumentation; no
  `pool.acquire` spans. It emits `db.system.name` (OTel semconv ≥ 1.33), which LiveProbe reads.
- **Redis** — `redisotel`. go-redis's `CLIENT MAINT_NOTIFICATIONS` handshake is disabled: OSS Redis
  rejects it and every new connection would otherwise record an errored span.
- **RabbitMQ** — no maintained instrumentation exists, so `shared/amqp.go` does it by hand: a producer
  span per publish with the trace context injected into message headers, and a consumer span per
  delivery in the extracted context. A checkout is one trace from the loadgen to the notification worker.
- **Client cancellation** doesn't cancel server work (as in Node), so a *slow* dependency shows as
  slow, not as instant `context canceled` errors on its datastore edges.

## Failure scenario (`docker-compose.chaos.yml`)

`CATALOG_FAULT_*` makes ~35% of product reads take 1.5–2 s; `ORDER_FAULT_*` fails ~35% of
`POST /orders` with 503. `CART_RESILIENCE_*` / `GATEWAY_RESILIENCE_*` add timeouts, retries with
backoff and a per-host circuit breaker. In LiveProbe: order red, catalog slow with callers timing out
and retrying, error groups pointing at cart's timeouts, alerts firing.

## Compatibility with the TS testbed

JSON contracts, routes, ports, env vars, database schemas and the RabbitMQ topology are identical.
Password hashes (scrypt, Node's parameters) and session JWTs (HS256) are interchangeable — verified
against artifacts produced by Node in `cmd/auth/auth_test.go`. The compose project is `shopwave-go`,
so its volumes don't collide with the TS stack's.

Not ported: the React storefront SPA (`--profile ui` in the TS testbed) — an optional demo shop UI,
not a traffic source; the loadgen drives the same journeys.
