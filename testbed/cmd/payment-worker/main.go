// Command payment-worker: charges each order.created — declines with PAYMENT_DECLINE_RATE, else
// completes — records the payment, then publishes the outcome. The row is written before the
// publish so the outcome is durable even if the process dies right after.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/rand/v2"
	"strconv"

	"shopwave/shared"
)

const schema = `
CREATE TABLE IF NOT EXISTS payments (
  id BIGSERIAL PRIMARY KEY,
  order_id UUID NOT NULL,
  amount_cents INTEGER NOT NULL,
  status TEXT NOT NULL,
  reason TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

func main() {
	ctx := context.Background()
	defer shared.InitTracing(ctx, "payment-worker")()
	pool := shared.NewPool(ctx, shared.Env("POSTGRES_DB", "payments"))
	shared.MustExec(ctx, pool, schema)
	rabbit := shared.ConnectRabbit()
	declineRate, err := strconv.ParseFloat(shared.Env("PAYMENT_DECLINE_RATE", "0.15"), 64)
	if err != nil {
		declineRate = 0.15
	}

	rabbit.Consume("payment.order-created", []string{shared.RKOrderCreated}, func(ctx context.Context, body []byte, _ string) error {
		var o shared.OrderCreated
		if err := json.Unmarshal(body, &o); err != nil || o.OrderID == "" {
			return errors.New("invalid OrderCreated payload")
		}
		declined := rand.Float64() < declineRate
		status, reason := "completed", any(nil)
		if declined {
			status, reason = "failed", "card_declined"
		}
		if _, err := pool.Exec(ctx, "INSERT INTO payments (order_id, amount_cents, status, reason) VALUES ($1, $2, $3, $4)", o.OrderID, o.TotalCents, status, reason); err != nil {
			return err
		}
		result := shared.PaymentResult{OrderID: o.OrderID, Status: status}
		rk := shared.RKPaymentCompleted
		if declined {
			result.Reason, rk = "card_declined", shared.RKPaymentFailed
		}
		log.Printf("payment %s order=%s", status, o.OrderID)
		return rabbit.Publish(ctx, rk, result)
	})

	shared.NewServer("payment-worker").Start(shared.EnvInt("PAYMENT_PORT", 3005), func() {
		rabbit.Close()
		pool.Close()
	})
}
