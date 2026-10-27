// Command catalog: products in Postgres behind a Redis read-through cache — the hit/miss path the
// sequence diagram shows. CATALOG_FAULT_* makes it a slow/failing dependency on demand.
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
	"github.com/redis/go-redis/v9"

	"shopwave/shared"
)

const schema = `
CREATE TABLE IF NOT EXISTS products (
  id           TEXT PRIMARY KEY,
  sku          TEXT NOT NULL UNIQUE,
  name         TEXT NOT NULL,
  description  TEXT NOT NULL,
  price_cents  INTEGER NOT NULL,
  currency     TEXT NOT NULL DEFAULT 'USD',
  category     TEXT NOT NULL,
  image_url    TEXT NOT NULL,
  stock        INTEGER NOT NULL DEFAULT 0,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);`

const cacheTTL = 60 * time.Second

func img(slug string) string { return "https://picsum.photos/seed/" + slug + "/400/400" }

var seed = []shared.Product{
	{ID: "p-001", SKU: "AUD-HP-01", Name: "Aria Wireless Headphones", Description: "Over-ear, 30h battery, ANC.", PriceCents: 18900, Currency: "USD", Category: "audio", ImageURL: img("aria"), Stock: 50},
	{ID: "p-002", SKU: "AUD-EB-02", Name: "Pulse Earbuds", Description: "In-ear, sweatproof, USB-C.", PriceCents: 8900, Currency: "USD", Category: "audio", ImageURL: img("pulse"), Stock: 120},
	{ID: "p-003", SKU: "COM-KB-01", Name: "Meridian Mechanical Keyboard", Description: "Hot-swap, 75%, RGB.", PriceCents: 12900, Currency: "USD", Category: "computing", ImageURL: img("meridian"), Stock: 40},
	{ID: "p-004", SKU: "COM-MS-02", Name: "Glide Wireless Mouse", Description: "Ergo, 8k DPI, silent click.", PriceCents: 5900, Currency: "USD", Category: "computing", ImageURL: img("glide"), Stock: 80},
	{ID: "p-005", SKU: "COM-MN-03", Name: "Vista 27\" 4K Monitor", Description: "IPS, 144Hz, USB-C 90W.", PriceCents: 42900, Currency: "USD", Category: "computing", ImageURL: img("vista"), Stock: 15},
	{ID: "p-006", SKU: "HOM-LM-01", Name: "Lumen Smart Lamp", Description: "Tunable white, app control.", PriceCents: 4900, Currency: "USD", Category: "home", ImageURL: img("lumen"), Stock: 60},
	{ID: "p-007", SKU: "HOM-MG-02", Name: "Roast Ceramic Mug", Description: "350ml, double-walled.", PriceCents: 1900, Currency: "USD", Category: "home", ImageURL: img("roast"), Stock: 200},
	{ID: "p-008", SKU: "WEA-WT-01", Name: "Tempo Fitness Watch", Description: "HR, GPS, 7-day battery.", PriceCents: 19900, Currency: "USD", Category: "wearables", ImageURL: img("tempo"), Stock: 35},
	{ID: "p-009", SKU: "WEA-BD-02", Name: "Stride Fitness Band", Description: "Sleep + steps, slim.", PriceCents: 6900, Currency: "USD", Category: "wearables", ImageURL: img("stride"), Stock: 90},
	{ID: "p-010", SKU: "PWR-PB-01", Name: "Volt 20k Power Bank", Description: "20000mAh, 65W PD.", PriceCents: 5900, Currency: "USD", Category: "power", ImageURL: img("volt"), Stock: 110},
	{ID: "p-011", SKU: "PWR-CH-02", Name: "Nimbus GaN Charger", Description: "100W, 3-port, foldable.", PriceCents: 6900, Currency: "USD", Category: "power", ImageURL: img("nimbus"), Stock: 70},
	{ID: "p-012", SKU: "AUD-SP-03", Name: "Echo Portable Speaker", Description: "IP67, 24h, stereo pair.", PriceCents: 9900, Currency: "USD", Category: "audio", ImageURL: img("echo"), Stock: 55},
}

const cols = "id, sku, name, description, price_cents, currency, category, image_url, stock"

func scanProducts(rows pgx.Rows) ([]shared.Product, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (shared.Product, error) {
		var p shared.Product
		err := r.Scan(&p.ID, &p.SKU, &p.Name, &p.Description, &p.PriceCents, &p.Currency, &p.Category, &p.ImageURL, &p.Stock)
		return p, err
	})
}

type repo struct {
	pool *pgxpool.Pool
	rdb  *redis.Client
}

func (c repo) list(ctx context.Context) ([]shared.Product, error) {
	rows, err := c.pool.Query(ctx, "SELECT "+cols+" FROM products ORDER BY name ASC")
	if err != nil {
		return nil, err
	}
	return scanProducts(rows)
}

// get is the read-through cache: Redis hit, or Postgres then a Redis SET.
func (c repo) get(ctx context.Context, id string) (*shared.Product, string, error) {
	key := "product:" + id
	if cached, err := c.rdb.Get(ctx, key).Result(); err == nil {
		var p shared.Product
		if json.Unmarshal([]byte(cached), &p) == nil {
			return &p, "hit", nil
		}
	} else if !errors.Is(err, redis.Nil) {
		return nil, "", err
	}
	rows, err := c.pool.Query(ctx, "SELECT "+cols+" FROM products WHERE id = $1", id)
	if err != nil {
		return nil, "", err
	}
	products, err := scanProducts(rows)
	if err != nil || len(products) == 0 {
		return nil, "miss", err
	}
	p := products[0]
	b, _ := json.Marshal(p)
	if err := c.rdb.Set(ctx, key, b, cacheTTL).Err(); err != nil {
		return nil, "", err
	}
	return &p, "miss", nil
}

func main() {
	ctx := context.Background()
	defer shared.InitTracing(ctx, "catalog")()
	pool := shared.NewPool(ctx, shared.Env("POSTGRES_DB", "catalog"))
	shared.MustExec(ctx, pool, schema)
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM products").Scan(&n); err == nil && n == 0 {
		for _, p := range seed {
			shared.MustExec(ctx, pool, "INSERT INTO products ("+cols+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)",
				p.ID, p.SKU, p.Name, p.Description, p.PriceCents, p.Currency, p.Category, p.ImageURL, p.Stock)
		}
	}
	c := repo{pool: pool, rdb: shared.NewRedis()}

	fault := shared.FaultFromEnv("CATALOG_FAULT")
	if fault.Active() {
		log.Printf("[catalog] fault injection ACTIVE: %+v", fault)
	}

	s := shared.NewServer("catalog")
	s.Handle("GET /products", func(w http.ResponseWriter, r *http.Request) error {
		if err := shared.ApplyFault(r.Context(), fault, shared.FaultDeps{}); err != nil {
			return err
		}
		products, err := c.list(r.Context())
		if err != nil {
			return err
		}
		return shared.WriteJSON(w, 200, map[string]any{"products": products})
	})
	s.Handle("GET /products/{id}", func(w http.ResponseWriter, r *http.Request) error {
		if err := shared.ApplyFault(r.Context(), fault, shared.FaultDeps{}); err != nil {
			return err
		}
		id := r.PathValue("id")
		p, cache, err := c.get(r.Context(), id)
		if err != nil {
			return err
		}
		if p == nil {
			return shared.WriteJSON(w, 404, map[string]string{"error": "product_not_found", "id": id})
		}
		w.Header().Set("x-cache", cache)
		return shared.WriteJSON(w, 200, map[string]any{"product": p, "cache": cache})
	})
	s.Start(shared.EnvInt("CATALOG_PORT", 3002), func() {
		pool.Close()
		_ = c.rdb.Close()
	})
}
