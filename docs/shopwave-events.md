# Shopwave: actions, events, and traces

Shopwave is the e-commerce testbed (`testbed/`, its own Go module `shopwave`). This doc covers
what each action does, the exact structure of every message it puts on RabbitMQ, and the trace
each action produces. Message contracts live in
[`testbed/shared/contracts.go`](../testbed/shared/contracts.go), so producers and consumers
can't drift. The JSON field names, routes, ports and RabbitMQ topology are identical to the TS
testbed's, so the two can be mixed. How to run it is in
[testbed/README.md](../testbed/README.md).

## HTTP API (through the gateway, `:3000`)

The gateway is the single entry point. Protected routes require `Authorization: Bearer <jwt>`.
The gateway validates the token by calling `auth GET /verify`, then forwards the request with an
`x-user-id` header.

| Method + path | Auth | Downstream | What it does |
|---------------|------|-----------|--------------|
| `POST /api/signup` | no | auth | create user (scrypt hash), issue JWT + Redis session |
| `POST /api/login` | no | auth | verify password, issue JWT + Redis session |
| `GET /api/products` | no | catalog | list products (Postgres) |
| `GET /api/products/{id}` | no | catalog | one product; Redis read-through cache (60s) |
| `GET /api/cart` | yes | cart → catalog | cart items priced via catalog, one call per line |
| `POST /api/cart/items` | yes | cart → catalog | validate product, `HINCRBY` qty in Redis |
| `DELETE /api/cart/items/{productId}` | yes | cart | `HDEL` from the Redis cart |
| `POST /api/checkout` | yes | cart → catalog, order | price the cart, `POST /orders`, clear the cart |
| `GET /api/orders` | yes | order | list the user's orders (Postgres) |
| `GET /api/orders/{id}` | yes | order | one order with items + status |

State stores:

- **auth**: users in Postgres, sessions in Redis (`session:<jti>` → userId, 1h).
- **catalog**: products in Postgres, cache `product:<id>` in Redis.
- **cart**: a Redis hash `cart:<userId>` of `productId → qty`.
- **order**: orders + order_items in Postgres.

Each service has its own database in the one Postgres container.

## RabbitMQ topology

One durable topic exchange, `shopwave.orders`. Routing keys and payloads:

```go
const OrderExchange = "shopwave.orders"

const (
	RKOrderCreated        = "order.created"
	RKPaymentCompleted    = "payment.completed"
	RKPaymentFailed       = "payment.failed"
	RKInventoryReserved   = "inventory.reserved"
	RKInventoryOutOfStock = "inventory.out_of_stock"
)
```

Queues and their bindings. Each consumer has its own durable queue, so `order.created` fans
out to both workers:

| Queue | Bound routing keys | Consumer |
|-------|--------------------|----------|
| `payment.order-created` | `order.created` | payment-worker |
| `inventory.order-created` | `order.created` | inventory-worker |
| `notification.events` | `payment.completed`, `payment.failed`, `inventory.reserved`, `inventory.out_of_stock` | notification-worker |
| `order.saga` | `payment.completed`, `payment.failed`, `inventory.reserved`, `inventory.out_of_stock` | order (the saga) |

Every message is published as JSON, `persistent`, with `content-type: application/json`.
Crucially, the W3C **traceparent is injected into the message headers**, so a consumer continues
the same trace as the publisher. Consumers process with prefetch 16. A handler error nacks the
message without requeue, so one poison message can't wedge a queue.

## Message payloads (exact structure)

### `order.created`: published by **order** on checkout

```go
type OrderCreated struct {
	OrderID    string      `json:"orderId"`    // UUID
	UserID     string      `json:"userId"`     // UUID
	Items      []OrderItem `json:"items"`      // {productId, quantity, priceCents}
	TotalCents int         `json:"totalCents"`
	Currency   string      `json:"currency"`   // "USD"
}
```
Example:
```json
{
  "orderId": "70edf0df-c85c-4feb-9d04-a9aa44ffa596",
  "userId": "5a139121-40c4-467b-9869-3d631dd6a882",
  "items": [
    { "productId": "p-003", "quantity": 2, "priceCents": 12900 },
    { "productId": "p-007", "quantity": 1, "priceCents": 1900 }
  ],
  "totalCents": 27700,
  "currency": "USD"
}
```

### `payment.completed` / `payment.failed`: published by **payment-worker**

```go
type PaymentResult struct {
	OrderID string `json:"orderId"`
	Status  string `json:"status"`           // "completed" | "failed"
	Reason  string `json:"reason,omitempty"` // set on failure, e.g. "card_declined"
}
```
```json
{ "orderId": "70edf0df-…", "status": "failed", "reason": "card_declined" }
```
The worker declines a configurable fraction of payments (`PAYMENT_DECLINE_RATE`, default 0.15)
and writes a row to its `payments` table either way.

### `inventory.reserved` / `inventory.out_of_stock`: published by **inventory-worker**

```go
type InventoryResult struct {
	OrderID   string      `json:"orderId"`
	Status    string      `json:"status"`              // "reserved" | "out_of_stock"
	Shortfall []Shortfall `json:"shortfall,omitempty"` // {productId, requested, available}
}
```
```json
{
  "orderId": "70edf0df-…",
  "status": "out_of_stock",
  "shortfall": [{ "productId": "p-005", "requested": 5, "available": 4 }]
}
```
The worker holds real stock in its `inventory` table. The table is seeded, and `p-005` and
`p-008` are low on purpose. When everything is available, the worker decrements stock inside a
transaction. Otherwise it reports the shortfall and reserves nothing.

## The checkout event flow (choreography saga)

There is no orchestrator. Services react to events, and the order service folds the results into
the order status.

```
POST /api/checkout
  gateway → cart → (catalog prices) → order POST /orders  [status: pending]
                                          │
                                          └─ publish order.created ──► shopwave.orders
                                                                          │ fan-out
                          ┌───────────────────────────────────────────────┤
                     payment-worker                             inventory-worker
                     pay (85% ok)                               reserve / out-of-stock
                          │ publish payment.completed|failed         │ publish inventory.reserved|out_of_stock
                          └───────────────► shopwave.orders ◄─────────┘
                                               │
                          ┌────────────────────┴────────────────────┐
                     order (saga)                             notification-worker
                     update order status                     dedupe (Redis) + "send" email
```

The saga recomputes **order status** in one atomic SQL `UPDATE` per event, so a payment event
and an inventory event can't race:

- `payment.failed` **or** `inventory.out_of_stock` → `cancelled`
- `payment.completed` **and** `inventory.reserved` → `confirmed`
- otherwise still `pending`

So `GET /api/orders/{id}` transitions `pending → confirmed` (both succeeded) or
`pending → cancelled` (either failed). Before logging the "email", the **notification-worker**
dedupes each `(orderId, routing key)` with a Redis `SET NX` (`notif:<orderId>:<key>`, 1h), so
retries don't double-send.

## How it is traced

Go has no auto-instrumentation, so every span comes from explicit instrumentation in
`testbed/shared`, shaped to read like the Node testbed's:

- **HTTP**: `otelhttp` on every server and client. Server spans are named by route pattern
  (`GET /products/{id}`) with `http.route` stamped after routing. Client spans are named
  `METHOD /path`. W3C `traceparent` is propagated on every call.
- **Postgres**: `otelpgx`, with spans named `pg.query:<VERB>` (`pg.query:SELECT`,
  `pg.query:UPDATE`, …) like the Node pg instrumentation, and no `pool.acquire` spans. These
  spans carry `db.system.name=postgresql`, which LiveProbe resolves to the `postgresql` peer
  (see [event-model.md](event-model.md#peer-resolution-outbound-spans-only)).
- **Redis**: `redisotel`, one client span per command.
- **RabbitMQ**: hand-written in `shared/amqp.go`. Each publish is a `producer` span
  (`publish <routing key>`) with the trace context injected into the message headers. Each
  delivery is a `consumer` span (`process <routing key>`) started in the extracted context.
  Both carry `messaging.system=rabbitmq`.
- **Client cancellation** doesn't cancel server work (as in Node). A *slow* dependency
  therefore shows as slow, not as instant `context canceled` errors on its datastore edges.

## What each action looks like as a trace

- **Browse a product** (`GET /api/products/{id}`): `gateway → catalog`, then in catalog a
  Redis `get` (cache) and, on a miss, a Postgres `SELECT` + Redis `set`. About 4–6 spans.
- **Add to cart** (`POST /api/cart/items`): `gateway → auth /verify (redis)` then
  `gateway → cart → catalog` (validate) + Redis `hincrby`.
- **Checkout** (`POST /api/checkout`): the big one. It is one connected trace of roughly 48–53
  spans across nine services: loadgen, gateway, auth, cart, catalog, order, Postgres, Redis, a
  RabbitMQ `publish`, then the consumer spans in payment-worker, inventory-worker, the order
  saga, and notification-worker. It stays one trace because trace context rides through the
  RabbitMQ headers. Cart prices each line with its own catalog call, an N+1 that the sequence
  diagram folds into a ×N group.

In LiveProbe, a checkout is a single sequence diagram spanning all of that. It contributes edges
like `gateway→cart`, `order→postgresql`, `order→rabbitmq` and `payment-worker→postgresql` to
the aggregate flow graph.

## Load generator

`testbed/cmd/loadgen` runs `CONCURRENCY` virtual users (default 5). Each one loops the full
journey (signup or login → list products → add 1–3 items → view cart → checkout → view orders)
with `THINK_MS` between steps. Its own requests are traced as service `loadgen`, so each journey
step is a trace rooted at the shopper. Payment declines and out-of-stock are built in, so the
loadgen naturally produces error traces and red edges. In compose it runs under the `load`
profile and waits until every service is healthy. `./dev-start.sh --testbed` starts it for you;
`--chaos` adds the failure scenario (slow catalog, flaky order, resilient callers). See
[testbed/README.md](../testbed/README.md).
