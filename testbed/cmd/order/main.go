// Command order: creates orders (header + items in one transaction), publishes order.created, and
// runs the choreography saga — folding payment and inventory outcomes into order status as the
// workers report back. Consumes run in the propagated trace context, so the whole checkout is one
// trace. ORDER_FAULT_* makes it a flaky dependency on demand.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"shopwave/shared"
)

const schema = `
CREATE TABLE IF NOT EXISTS orders (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  total_cents INTEGER NOT NULL,
  currency TEXT NOT NULL DEFAULT 'USD',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS order_items (
  id BIGSERIAL PRIMARY KEY,
  order_id UUID NOT NULL REFERENCES orders(id),
  product_id TEXT NOT NULL,
  quantity INTEGER NOT NULL,
  price_cents INTEGER NOT NULL
);
ALTER TABLE orders ADD COLUMN IF NOT EXISTS payment_status TEXT;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS inventory_status TEXT;`

type order struct {
	ID         string             `json:"id"`
	UserID     string             `json:"userId"`
	Status     string             `json:"status"`
	TotalCents int                `json:"totalCents"`
	Currency   string             `json:"currency"`
	Items      []shared.OrderItem `json:"items"`
	CreatedAt  string             `json:"createdAt"`
}

const orderCols = "id::text, user_id::text, status, total_cents, currency, created_at"

func scanOrder(row pgx.Row) (order, error) {
	var o order
	var created time.Time
	err := row.Scan(&o.ID, &o.UserID, &o.Status, &o.TotalCents, &o.Currency, &created)
	o.CreatedAt = created.UTC().Format("2006-01-02T15:04:05.000Z") // JS toISOString
	o.Items = []shared.OrderItem{}
	return o, err
}

type repo struct{ pool *pgxpool.Pool }

// create writes the header and items together so a partial order is never observable.
func (r repo) create(ctx context.Context, userID string, items []shared.OrderItem, total int, currency string) (order, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return order{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	o, err := scanOrder(tx.QueryRow(ctx, "INSERT INTO orders (user_id, total_cents, currency) VALUES ($1, $2, $3) RETURNING "+orderCols, userID, total, currency))
	if err != nil {
		return order{}, err
	}
	for _, it := range items {
		if _, err := tx.Exec(ctx, "INSERT INTO order_items (order_id, product_id, quantity, price_cents) VALUES ($1, $2, $3, $4)", o.ID, it.ProductID, it.Quantity, it.PriceCents); err != nil {
			return order{}, err
		}
	}
	o.Items = items
	return o, tx.Commit(ctx)
}

// Saga updates recompute status in one atomic UPDATE so a payment and an inventory event for the
// same order cannot race a read-modify-write.
func (r repo) applyPayment(ctx context.Context, orderID, status string) error {
	_, err := r.pool.Exec(ctx, `UPDATE orders SET payment_status = $2,
	  status = CASE WHEN $2 = 'failed' OR inventory_status = 'out_of_stock' THEN 'cancelled'
	                WHEN $2 = 'completed' AND inventory_status = 'reserved' THEN 'confirmed'
	                ELSE 'pending' END
	  WHERE id = $1`, orderID, status)
	return err
}

func (r repo) applyInventory(ctx context.Context, orderID, status string) error {
	_, err := r.pool.Exec(ctx, `UPDATE orders SET inventory_status = $2,
	  status = CASE WHEN $2 = 'out_of_stock' OR payment_status = 'failed' THEN 'cancelled'
	                WHEN $2 = 'reserved' AND payment_status = 'completed' THEN 'confirmed'
	                ELSE 'pending' END
	  WHERE id = $1`, orderID, status)
	return err
}

func (r repo) attachItems(ctx context.Context, orders []order) error {
	if len(orders) == 0 {
		return nil
	}
	ids := make([]string, len(orders))
	index := map[string]int{}
	for i, o := range orders {
		ids[i], index[o.ID] = o.ID, i
	}
	rows, err := r.pool.Query(ctx, "SELECT order_id::text, product_id, quantity, price_cents FROM order_items WHERE order_id = ANY($1::uuid[]) ORDER BY id ASC", ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid string
		var it shared.OrderItem
		if err := rows.Scan(&oid, &it.ProductID, &it.Quantity, &it.PriceCents); err != nil {
			return err
		}
		orders[index[oid]].Items = append(orders[index[oid]].Items, it)
	}
	return rows.Err()
}

func (r repo) get(ctx context.Context, id string) (*order, error) {
	o, err := scanOrder(r.pool.QueryRow(ctx, "SELECT "+orderCols+" FROM orders WHERE id::text = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	list := []order{o}
	if err := r.attachItems(ctx, list); err != nil {
		return nil, err
	}
	return &list[0], nil
}

func (r repo) listByUser(ctx context.Context, userID string) ([]order, error) {
	q, args := "SELECT "+orderCols+" FROM orders ORDER BY created_at DESC", []any{}
	if userID != "" {
		q, args = "SELECT "+orderCols+" FROM orders WHERE user_id::text = $1 ORDER BY created_at DESC", []any{userID}
	}
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	orders, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (order, error) { return scanOrder(row) })
	if err != nil {
		return nil, err
	}
	return orders, r.attachItems(ctx, orders)
}

// validItems mirrors the TS isItem check: productId string, quantity and priceCents numbers.
func validItems(raw any) ([]shared.OrderItem, bool) {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	items := make([]shared.OrderItem, 0, len(list))
	for _, x := range list {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, false
		}
		pid, ok1 := m["productId"].(string)
		qty, ok2 := m["quantity"].(float64)
		price, ok3 := m["priceCents"].(float64)
		if !ok1 || !ok2 || !ok3 {
			return nil, false
		}
		items = append(items, shared.OrderItem{ProductID: pid, Quantity: int(qty), PriceCents: int(price)})
	}
	return items, true
}

func main() {
	ctx := context.Background()
	defer shared.InitTracing(ctx, "order")()
	pool := shared.NewPool(ctx, shared.Env("POSTGRES_DB", "orders"))
	shared.MustExec(ctx, pool, schema)
	rabbit := shared.ConnectRabbit()
	repo := repo{pool: pool}

	// Saga: payment and inventory outcomes fold into order status.
	rabbit.Consume("order.saga", []string{shared.RKPaymentCompleted, shared.RKPaymentFailed, shared.RKInventoryReserved, shared.RKInventoryOutOfStock},
		func(ctx context.Context, body []byte, rk string) error {
			var msg struct {
				OrderID string `json:"orderId"`
			}
			if json.Unmarshal(body, &msg) != nil || msg.OrderID == "" {
				return nil
			}
			switch rk {
			case shared.RKPaymentCompleted:
				return repo.applyPayment(ctx, msg.OrderID, "completed")
			case shared.RKPaymentFailed:
				return repo.applyPayment(ctx, msg.OrderID, "failed")
			case shared.RKInventoryReserved:
				return repo.applyInventory(ctx, msg.OrderID, "reserved")
			case shared.RKInventoryOutOfStock:
				return repo.applyInventory(ctx, msg.OrderID, "out_of_stock")
			}
			return nil
		})

	fault := shared.FaultFromEnv("ORDER_FAULT")
	if fault.Active() {
		log.Printf("[order] fault injection ACTIVE: %+v", fault)
	}

	s := shared.NewServer("order")
	s.Handle("POST /orders", func(w http.ResponseWriter, r *http.Request) error {
		// Fault before any DB work so a failed request never leaves a half-written order.
		if err := shared.ApplyFault(r.Context(), fault, shared.FaultDeps{}); err != nil {
			return err
		}
		var body map[string]any
		if err := shared.ReadJSON(r, &body); err != nil {
			return err
		}
		userID, okUser := body["userId"].(string)
		items, okItems := validItems(body["items"])
		if !okUser || !okItems {
			return shared.Fail(400, "invalid_input")
		}
		total, _ := body["totalCents"].(float64)
		currency, ok := body["currency"].(string)
		if !ok {
			currency = "USD"
		}
		o, err := repo.create(r.Context(), userID, items, int(total), currency)
		if err != nil {
			return err
		}
		event := shared.OrderCreated{OrderID: o.ID, UserID: o.UserID, Items: o.Items, TotalCents: o.TotalCents, Currency: o.Currency}
		if err := rabbit.Publish(r.Context(), shared.RKOrderCreated, event); err != nil {
			return err
		}
		return shared.WriteJSON(w, 201, map[string]any{"order": o})
	})
	s.Handle("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) error {
		o, err := repo.get(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		if o == nil {
			return shared.Fail(404, "order_not_found")
		}
		return shared.WriteJSON(w, 200, map[string]any{"order": o})
	})
	s.Handle("GET /orders", func(w http.ResponseWriter, r *http.Request) error {
		orders, err := repo.listByUser(r.Context(), r.URL.Query().Get("userId"))
		if err != nil {
			return err
		}
		return shared.WriteJSON(w, 200, map[string]any{"orders": orders})
	})

	s.Start(shared.EnvInt("ORDER_PORT", 3004), func() {
		rabbit.Close()
		pool.Close()
	})
}
