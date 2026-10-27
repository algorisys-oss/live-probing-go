// Command inventory-worker: reserves stock for each order.created atomically — all items or
// none — and publishes reserved / out_of_stock. p-005 and p-008 are deliberately low so
// out-of-stock happens under load.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"shopwave/shared"
)

const schema = `
CREATE TABLE IF NOT EXISTS inventory (
  product_id TEXT PRIMARY KEY,
  available  INTEGER NOT NULL
);`

var seed = []struct {
	id        string
	available int
}{
	{"p-001", 100}, {"p-002", 100}, {"p-003", 100}, {"p-004", 100}, {"p-005", 4}, {"p-006", 100},
	{"p-007", 100}, {"p-008", 6}, {"p-009", 100}, {"p-010", 100}, {"p-011", 100}, {"p-012", 100},
}

// reserve checks every item and only if all are fully in stock decrements them; otherwise nothing
// changes and the shortfall is reported. A missing product counts as available=0.
func reserve(ctx context.Context, pool *pgxpool.Pool, o shared.OrderCreated) (shared.InventoryResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return shared.InventoryResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var shortfall []shared.Shortfall
	for _, it := range o.Items {
		var available int
		err := tx.QueryRow(ctx, "SELECT available FROM inventory WHERE product_id = $1", it.ProductID).Scan(&available)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return shared.InventoryResult{}, err
		}
		if available < it.Quantity {
			shortfall = append(shortfall, shared.Shortfall{ProductID: it.ProductID, Requested: it.Quantity, Available: available})
		}
	}
	if len(shortfall) > 0 {
		return shared.InventoryResult{OrderID: o.OrderID, Status: "out_of_stock", Shortfall: shortfall}, nil
	}
	for _, it := range o.Items {
		if _, err := tx.Exec(ctx, "UPDATE inventory SET available = available - $1 WHERE product_id = $2", it.Quantity, it.ProductID); err != nil {
			return shared.InventoryResult{}, err
		}
	}
	return shared.InventoryResult{OrderID: o.OrderID, Status: "reserved"}, tx.Commit(ctx)
}

func main() {
	ctx := context.Background()
	defer shared.InitTracing(ctx, "inventory-worker")()
	pool := shared.NewPool(ctx, shared.Env("POSTGRES_DB", "inventory"))
	shared.MustExec(ctx, pool, schema)
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM inventory").Scan(&n); err == nil && n == 0 {
		for _, s := range seed {
			shared.MustExec(ctx, pool, "INSERT INTO inventory (product_id, available) VALUES ($1, $2) ON CONFLICT (product_id) DO NOTHING", s.id, s.available)
		}
	}
	rabbit := shared.ConnectRabbit()

	rabbit.Consume("inventory.order-created", []string{shared.RKOrderCreated}, func(ctx context.Context, body []byte, _ string) error {
		var o shared.OrderCreated
		if err := json.Unmarshal(body, &o); err != nil {
			return err
		}
		result, err := reserve(ctx, pool, o)
		if err != nil {
			return err
		}
		log.Printf("inventory %s order=%s", result.Status, result.OrderID)
		if result.Status == "reserved" {
			return rabbit.Publish(ctx, shared.RKInventoryReserved, shared.InventoryResult{OrderID: result.OrderID, Status: "reserved"})
		}
		return rabbit.Publish(ctx, shared.RKInventoryOutOfStock, result)
	})

	shared.NewServer("inventory-worker").Start(shared.EnvInt("INVENTORY_PORT", 3006), func() {
		rabbit.Close()
		pool.Close()
	})
}
