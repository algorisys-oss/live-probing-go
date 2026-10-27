// Command cart: carts as Redis hashes, priced against live catalog data. Building a cart view
// fetches each product from catalog one at a time — the N+1 LiveProbe folds into a ×N group.
// CART_RESILIENCE_* adds timeout/retry/breaker to cart's calls to catalog and order.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/redis/go-redis/v9"

	"shopwave/shared"
)

type cartItem struct {
	ProductID      string `json:"productId"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	PriceCents     int    `json:"priceCents"`
	LineTotalCents int    `json:"lineTotalCents"`
}

type cartView struct {
	Items      []cartItem `json:"items"`
	TotalCents int        `json:"totalCents"`
	Currency   string     `json:"currency"`
}

type service struct {
	rdb        *redis.Client
	http       *shared.ResilientClient
	policy     shared.Policy
	catalogURL string
	orderURL   string
}

func cartKey(userID string) string { return "cart:" + userID }

// fetchProduct returns nil for an unknown product (catalog 404).
func (s *service) fetchProduct(ctx context.Context, id string) (*shared.Product, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.catalogURL+"/products/"+url.PathEscape(id), nil)
	res, err := s.http.Do(req, s.policy)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return nil, nil
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("catalog %d for product %s", res.StatusCode, id)
	}
	var body struct {
		Product shared.Product `json:"product"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	return &body.Product, nil
}

// lines are the cart's (productId, quantity) pairs in a stable order.
func (s *service) lines(ctx context.Context, userID string) ([][2]string, error) {
	h, err := s.rdb.HGetAll(ctx, cartKey(userID)).Result()
	if err != nil {
		return nil, err
	}
	out := make([][2]string, 0, len(h))
	for k, v := range h {
		out = append(out, [2]string{k, v})
	}
	slices.SortFunc(out, func(a, b [2]string) int { return cmpString(a[0], b[0]) })
	return out, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// view joins stored quantities against live catalog prices; vanished products are skipped.
func (s *service) view(ctx context.Context, userID string) (cartView, error) {
	v := cartView{Items: []cartItem{}, Currency: "USD"}
	lines, err := s.lines(ctx, userID)
	if err != nil {
		return v, err
	}
	for _, l := range lines {
		p, err := s.fetchProduct(ctx, l[0])
		if err != nil {
			return v, err
		}
		if p == nil {
			continue
		}
		qty, _ := strconv.Atoi(l[1])
		line := p.PriceCents * qty
		v.TotalCents += line
		v.Items = append(v.Items, cartItem{ProductID: l[0], Name: p.Name, Quantity: qty, PriceCents: p.PriceCents, LineTotalCents: line})
	}
	return v, nil
}

// user is the authenticated user forwarded by the gateway; missing means it never passed auth.
func user(r *http.Request) (string, error) {
	if u := r.Header.Get("x-user-id"); u != "" {
		return u, nil
	}
	return "", shared.Fail(401, "unauthorized")
}

func main() {
	ctx := context.Background()
	defer shared.InitTracing(ctx, "cart")()
	svc := &service{
		rdb: shared.NewRedis(), http: shared.NewHTTPClient(), policy: shared.PolicyFromEnv("CART_RESILIENCE"),
		catalogURL: shared.Env("CATALOG_URL", "http://localhost:3002"), orderURL: shared.Env("ORDER_URL", "http://localhost:3004"),
	}

	s := shared.NewServer("cart")
	s.Handle("GET /cart", func(w http.ResponseWriter, r *http.Request) error {
		u, err := user(r)
		if err != nil {
			return err
		}
		v, err := svc.view(r.Context(), u)
		if err != nil {
			return err
		}
		return shared.WriteJSON(w, 200, v)
	})

	s.Handle("POST /cart/items", func(w http.ResponseWriter, r *http.Request) error {
		u, err := user(r)
		if err != nil {
			return err
		}
		var body map[string]any
		if err := shared.ReadJSON(r, &body); err != nil {
			return err
		}
		productID, _ := body["productId"].(string)
		qty, isNum := body["quantity"].(float64)
		if productID == "" || !isNum || qty != math.Trunc(qty) || qty < 1 {
			return shared.Fail(400, "invalid_input")
		}
		p, err := svc.fetchProduct(r.Context(), productID)
		if err != nil {
			return err
		}
		if p == nil {
			return shared.Fail(404, "product_not_found")
		}
		if err := svc.rdb.HIncrBy(r.Context(), cartKey(u), productID, int64(qty)).Err(); err != nil {
			return err
		}
		v, err := svc.view(r.Context(), u)
		if err != nil {
			return err
		}
		return shared.WriteJSON(w, 200, v)
	})

	s.Handle("DELETE /cart/items/{productId}", func(w http.ResponseWriter, r *http.Request) error {
		u, err := user(r)
		if err != nil {
			return err
		}
		if err := svc.rdb.HDel(r.Context(), cartKey(u), r.PathValue("productId")).Err(); err != nil {
			return err
		}
		v, err := svc.view(r.Context(), u)
		if err != nil {
			return err
		}
		return shared.WriteJSON(w, 200, v)
	})

	s.Handle("POST /cart/checkout", func(w http.ResponseWriter, r *http.Request) error {
		u, err := user(r)
		if err != nil {
			return err
		}
		lines, err := svc.lines(r.Context(), u)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return shared.Fail(400, "empty_cart")
		}
		items := []shared.OrderItem{}
		total := 0
		for _, l := range lines {
			p, err := svc.fetchProduct(r.Context(), l[0])
			if err != nil {
				return err
			}
			if p == nil {
				continue
			}
			qty, _ := strconv.Atoi(l[1])
			total += p.PriceCents * qty
			items = append(items, shared.OrderItem{ProductID: l[0], Quantity: qty, PriceCents: p.PriceCents})
		}
		payload, _ := json.Marshal(map[string]any{"userId": u, "items": items, "totalCents": total, "currency": "USD"})
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, svc.orderURL+"/orders", bytes.NewReader(payload))
		req.Header.Set("content-type", "application/json")
		res, err := svc.http.Do(req, svc.policy)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		w.Header().Set("content-type", firstNonEmpty(res.Header.Get("content-type"), "application/json"))
		if res.StatusCode < 200 || res.StatusCode > 299 {
			w.WriteHeader(res.StatusCode) // surface the order service's failure verbatim
			_, err = w.Write(body)
			return err
		}
		if err := svc.rdb.Del(r.Context(), cartKey(u)).Err(); err != nil {
			return err
		}
		w.WriteHeader(201)
		_, err = w.Write(body)
		return err
	})

	s.Start(shared.EnvInt("CART_PORT", 3003), func() { _ = svc.rdb.Close() })
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
