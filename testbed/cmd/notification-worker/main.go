// Command notification-worker: tells the customer about payment and inventory outcomes, once.
// A Redis SET NX dedupe key (1h) means a redelivered or double-published event never notifies
// twice — and puts a Redis hop on this worker's part of the trace.
package main

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"time"

	"shopwave/shared"
)

// describe returns the notice for an event, or ok=false when it carries no orderId.
func describe(body []byte, rk string) (orderID, message string, ok bool) {
	var e struct {
		OrderID   string             `json:"orderId"`
		Reason    string             `json:"reason"`
		Shortfall []shared.Shortfall `json:"shortfall"`
	}
	if json.Unmarshal(body, &e) != nil || e.OrderID == "" {
		return "", "", false
	}
	switch rk {
	case shared.RKPaymentCompleted:
		return e.OrderID, "order " + e.OrderID + ": payment confirmed", true
	case shared.RKPaymentFailed:
		reason := e.Reason
		if reason == "" {
			reason = "unknown reason"
		}
		return e.OrderID, "order " + e.OrderID + ": payment declined (" + reason + ")", true
	case shared.RKInventoryReserved:
		return e.OrderID, "order " + e.OrderID + ": items reserved", true
	case shared.RKInventoryOutOfStock:
		suffix := ""
		if len(e.Shortfall) > 0 {
			suffix = " (" + strconv.Itoa(len(e.Shortfall)) + " item(s) short)"
		}
		return e.OrderID, "order " + e.OrderID + ": some items are out of stock" + suffix, true
	}
	return "", "", false
}

func main() {
	ctx := context.Background()
	defer shared.InitTracing(ctx, "notification-worker")()
	rdb := shared.NewRedis()
	rabbit := shared.ConnectRabbit()

	rabbit.Consume("notification.events", []string{shared.RKPaymentCompleted, shared.RKPaymentFailed, shared.RKInventoryReserved, shared.RKInventoryOutOfStock},
		func(ctx context.Context, body []byte, rk string) error {
			orderID, message, ok := describe(body, rk)
			if !ok {
				log.Printf("notification: ignoring event without orderId (%s)", rk)
				return nil
			}
			key := "notif:" + orderID + ":" + rk
			acquired, err := rdb.SetNX(ctx, key, "1", time.Hour).Result()
			if err != nil {
				return err
			}
			if !acquired {
				log.Printf("notification: duplicate skipped %s", key)
				return nil
			}
			log.Printf("notification sent -> %s", message)
			return nil
		})

	shared.NewServer("notification-worker").Start(shared.EnvInt("NOTIFICATION_PORT", 3007), func() {
		rabbit.Close()
		_ = rdb.Close()
	})
}
