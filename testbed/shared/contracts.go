package shared

// Shared domain types and message contracts, so producers and consumers of the RabbitMQ events
// cannot drift. JSON field names match the TS testbed's, so mixed TS/Go deployments interoperate.

type Product struct {
	ID          string `json:"id"`
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceCents  int    `json:"priceCents"`
	Currency    string `json:"currency"`
	Category    string `json:"category"`
	ImageURL    string `json:"imageUrl"`
	Stock       int    `json:"stock"`
}

// OrderExchange is the single topic exchange; routing keys name the events.
const OrderExchange = "shopwave.orders"

const (
	RKOrderCreated        = "order.created"
	RKPaymentCompleted    = "payment.completed"
	RKPaymentFailed       = "payment.failed"
	RKInventoryReserved   = "inventory.reserved"
	RKInventoryOutOfStock = "inventory.out_of_stock"
)

type OrderItem struct {
	ProductID  string `json:"productId"`
	Quantity   int    `json:"quantity"`
	PriceCents int    `json:"priceCents"`
}

type OrderCreated struct {
	OrderID    string      `json:"orderId"`
	UserID     string      `json:"userId"`
	Items      []OrderItem `json:"items"`
	TotalCents int         `json:"totalCents"`
	Currency   string      `json:"currency"`
}

type PaymentResult struct {
	OrderID string `json:"orderId"`
	Status  string `json:"status"` // completed | failed
	Reason  string `json:"reason,omitempty"`
}

type Shortfall struct {
	ProductID string `json:"productId"`
	Requested int    `json:"requested"`
	Available int    `json:"available"`
}

type InventoryResult struct {
	OrderID   string      `json:"orderId"`
	Status    string      `json:"status"` // reserved | out_of_stock
	Shortfall []Shortfall `json:"shortfall,omitempty"`
}
