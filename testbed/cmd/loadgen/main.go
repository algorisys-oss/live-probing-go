// Command loadgen: CONCURRENCY virtual users looping a realistic shopper journey against the
// gateway — signup (or login), browse, add 1-3 items, view cart, checkout, view orders — with
// THINK_MS between steps. Its own requests are traced (service "loadgen"), so each journey step is
// a trace rooted at the shopper. DURATION_SECONDS=0 runs until stopped.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"shopwave/shared"
)

const password = "loadtestpw"

type stats struct{ journeys, checkouts, errors, requests atomic.Int64 }

type client struct {
	base  string
	http  *shared.ResilientClient
	stats *stats
}

func (c *client) do(ctx context.Context, method, path, token string, body any) (*http.Response, error) {
	c.stats.requests.Add(1)
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	return c.http.Do(req, shared.NoResilience)
}

// call performs a request, requires a 2xx, and decodes the body into out (if non-nil).
func (c *client) call(ctx context.Context, step, method, path, token string, body, out any) error {
	res, err := c.do(ctx, method, path, token, body)
	if err != nil {
		return fmt.Errorf("%s: %w", step, err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("%s failed: %d", step, res.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	_, err = io.Copy(io.Discard, res.Body)
	return err
}

type auth struct {
	Token string `json:"token"`
}

func (c *client) journey(ctx context.Context, n int, think time.Duration) error {
	email := fmt.Sprintf("vu-%06x-%d@load.test", rand.IntN(1<<24), n)
	var a auth
	res, err := c.do(ctx, "POST", "/api/signup", "", map[string]string{"email": email, "password": password})
	if err != nil {
		return fmt.Errorf("signup: %w", err)
	}
	switch {
	case res.StatusCode == 409:
		res.Body.Close()
		if err := c.call(ctx, "login", "POST", "/api/login", "", map[string]string{"email": email, "password": password}, &a); err != nil {
			return err
		}
	case res.StatusCode >= 200 && res.StatusCode < 300:
		err := json.NewDecoder(res.Body).Decode(&a)
		res.Body.Close()
		if err != nil {
			return err
		}
	default:
		res.Body.Close()
		return fmt.Errorf("signup failed: %d", res.StatusCode)
	}
	sleep(ctx, think)

	var catalog struct {
		Products []shared.Product `json:"products"`
	}
	if err := c.call(ctx, "list products", "GET", "/api/products", "", nil, &catalog); err != nil {
		return err
	}
	if len(catalog.Products) == 0 {
		return fmt.Errorf("no products available")
	}
	sleep(ctx, think)

	picks := rand.Perm(len(catalog.Products))[:min(len(catalog.Products), 1+rand.IntN(3))]
	for _, i := range picks {
		body := map[string]any{"productId": catalog.Products[i].ID, "quantity": 1 + rand.IntN(3)}
		if err := c.call(ctx, "add to cart", "POST", "/api/cart/items", a.Token, body, nil); err != nil {
			return err
		}
		sleep(ctx, think)
	}
	if err := c.call(ctx, "get cart", "GET", "/api/cart", a.Token, nil, nil); err != nil {
		return err
	}
	sleep(ctx, think)
	if err := c.call(ctx, "checkout", "POST", "/api/checkout", a.Token, nil, nil); err != nil {
		return err
	}
	c.stats.checkouts.Add(1)
	sleep(ctx, think)
	return c.call(ctx, "get orders", "GET", "/api/orders", a.Token, nil, nil)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func main() {
	defer shared.InitTracing(context.Background(), "loadgen")()
	target := strings.TrimRight(shared.Env("TARGET_URL", "http://localhost:3000"), "/")
	concurrency := shared.EnvInt("CONCURRENCY", 5)
	duration := time.Duration(shared.EnvInt("DURATION_SECONDS", 0)) * time.Second
	think := time.Duration(shared.EnvInt("THINK_MS", 400)) * time.Millisecond
	log.Printf("loadgen starting: target=%s concurrency=%d duration=%v think=%v", target, concurrency, duration, think)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}

	st := &stats{}
	c := &client{base: target, http: shared.NewHTTPClient(), stats: st}
	start := time.Now()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		last, lastAt := int64(0), start
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				req := st.requests.Load()
				log.Printf("stats: journeys=%d checkouts_ok=%d errors=%d rps~%.1f", st.journeys.Load(), st.checkouts.Load(), st.errors.Load(), float64(req-last)/now.Sub(lastAt).Seconds())
				last, lastAt = req, now
			}
		}
	}()

	var wg sync.WaitGroup
	for vu := range concurrency {
		wg.Go(func() {
			for n := 0; ctx.Err() == nil; n++ {
				if err := c.journey(ctx, n, think); err != nil {
					if ctx.Err() != nil {
						return
					}
					st.errors.Add(1)
					log.Printf("vu=%d journey error: %v", vu, err)
				} else {
					st.journeys.Add(1)
				}
				sleep(ctx, think)
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	log.Printf("SUMMARY: elapsed=%.1fs journeys=%d checkouts_ok=%d errors=%d requests=%d rps~%.1f",
		elapsed, st.journeys.Load(), st.checkouts.Load(), st.errors.Load(), st.requests.Load(), float64(st.requests.Load())/elapsed)
	os.Exit(0)
}
