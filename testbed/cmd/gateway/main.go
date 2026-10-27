// Command gateway: the public API. Authenticates via the auth service and forwards to catalog,
// cart and order; every outbound call is a client span with the traceparent injected, so a request
// is one trace across all services. GATEWAY_RESILIENCE_* adds timeout/retry/breaker.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"shopwave/shared"
)

type gateway struct {
	http                                   *shared.ResilientClient
	policy                                 shared.Policy
	authURL, catalogURL, cartURL, orderURL string
}

// forward relays a request downstream and copies status, content-type, x-cache and body back.
func (g *gateway) forward(w http.ResponseWriter, r *http.Request, method, target, userID string, withBody bool) error {
	var body io.Reader
	var payload []byte
	if withBody {
		var err error
		if payload, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20)); err != nil {
			return shared.Fail(400, "invalid_input")
		}
		body = bytes.NewReader(payload)
	}
	req, _ := http.NewRequestWithContext(r.Context(), method, target, body)
	if withBody {
		req.Header.Set("content-type", "application/json")
	}
	if userID != "" {
		req.Header.Set("x-user-id", userID)
	}
	res, err := g.http.Do(req, g.policy)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	ct := res.Header.Get("content-type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("content-type", ct)
	if cache := res.Header.Get("x-cache"); cache != "" {
		w.Header().Set("x-cache", cache)
	}
	w.WriteHeader(res.StatusCode)
	_, err = io.Copy(w, res.Body)
	return err
}

// authenticate asks the auth service to validate the bearer token. The extra hop keeps the
// gateway stateless and shows auth in every protected trace.
func (g *gateway) authenticate(r *http.Request) (string, error) {
	authorization := r.Header.Get("authorization")
	if authorization == "" {
		return "", shared.Fail(401, "unauthorized")
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, g.authURL+"/verify", nil)
	req.Header.Set("authorization", authorization)
	res, err := g.http.Do(req, g.policy)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", shared.Fail(401, "unauthorized")
	}
	var body struct {
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.UserID == "" {
		return "", shared.Fail(401, "unauthorized")
	}
	return body.UserID, nil
}

// protected wraps a handler that needs an authenticated user.
func (g *gateway) protected(next func(w http.ResponseWriter, r *http.Request, userID string) error) shared.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		userID, err := g.authenticate(r)
		if err != nil {
			return err
		}
		return next(w, r, userID)
	}
}

// cors is open: the storefront SPA is served from another origin. Fine for a local testbed.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("access-control-allow-origin", "*")
		w.Header().Set("access-control-allow-headers", "authorization,content-type")
		w.Header().Set("access-control-allow-methods", "GET,POST,DELETE,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	ctx := context.Background()
	defer shared.InitTracing(ctx, "gateway")()
	g := &gateway{
		http: shared.NewHTTPClient(), policy: shared.PolicyFromEnv("GATEWAY_RESILIENCE"),
		authURL:    shared.Env("AUTH_URL", "http://localhost:3001"),
		catalogURL: shared.Env("CATALOG_URL", "http://localhost:3002"),
		cartURL:    shared.Env("CART_URL", "http://localhost:3003"),
		orderURL:   shared.Env("ORDER_URL", "http://localhost:3004"),
	}
	esc := url.PathEscape

	s := shared.NewServer("gateway")
	s.Middleware = cors
	// public
	s.Handle("POST /api/signup", func(w http.ResponseWriter, r *http.Request) error {
		return g.forward(w, r, "POST", g.authURL+"/signup", "", true)
	})
	s.Handle("POST /api/login", func(w http.ResponseWriter, r *http.Request) error {
		return g.forward(w, r, "POST", g.authURL+"/login", "", true)
	})
	s.Handle("GET /api/products", func(w http.ResponseWriter, r *http.Request) error {
		return g.forward(w, r, "GET", g.catalogURL+"/products", "", false)
	})
	s.Handle("GET /api/products/{id}", func(w http.ResponseWriter, r *http.Request) error {
		return g.forward(w, r, "GET", g.catalogURL+"/products/"+esc(r.PathValue("id")), "", false)
	})
	// protected: cart
	s.Handle("GET /api/cart", g.protected(func(w http.ResponseWriter, r *http.Request, u string) error {
		return g.forward(w, r, "GET", g.cartURL+"/cart", u, false)
	}))
	s.Handle("POST /api/cart/items", g.protected(func(w http.ResponseWriter, r *http.Request, u string) error {
		return g.forward(w, r, "POST", g.cartURL+"/cart/items", u, true)
	}))
	s.Handle("DELETE /api/cart/items/{productId}", g.protected(func(w http.ResponseWriter, r *http.Request, u string) error {
		return g.forward(w, r, "DELETE", g.cartURL+"/cart/items/"+esc(r.PathValue("productId")), u, false)
	}))
	s.Handle("POST /api/checkout", g.protected(func(w http.ResponseWriter, r *http.Request, u string) error {
		return g.forward(w, r, "POST", g.cartURL+"/cart/checkout", u, false)
	}))
	// protected: orders
	s.Handle("GET /api/orders", g.protected(func(w http.ResponseWriter, r *http.Request, u string) error {
		return g.forward(w, r, "GET", g.orderURL+"/orders?userId="+url.QueryEscape(u), u, false)
	}))
	s.Handle("GET /api/orders/{id}", g.protected(func(w http.ResponseWriter, r *http.Request, u string) error {
		return g.forward(w, r, "GET", g.orderURL+"/orders/"+esc(r.PathValue("id")), u, false)
	}))

	s.Start(shared.EnvInt("GATEWAY_PORT", 3000), nil)
}
