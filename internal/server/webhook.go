package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"liveprobe/internal/core"
)

// FiringAlert is an active alert stamped with when it started firing (epoch ms).
type FiringAlert = core.FiringAlert

type WebhookOptions struct {
	URL      string
	Timeout  time.Duration // per attempt
	Retries  int           // extra attempts after the first
	Backoff  time.Duration // doubled each retry
	Cooldown time.Duration // min gap between firing the same alert id again
	// Seams for tests / custom transports.
	Client  *http.Client
	Now     func() int64 // epoch ms
	Sleep   func(time.Duration)
	OnError func(error, FiringAlert)
	OnSent  func(FiringAlert)
}

func DefaultWebhookOptions(url string) WebhookOptions {
	return WebhookOptions{URL: url, Timeout: 5 * time.Second, Retries: 2, Backoff: 500 * time.Millisecond, Cooldown: 60 * time.Second}
}

// WebhookNotifier delivers one JSON POST per inactive→active alert transition, with a per-attempt
// timeout, exponential-backoff retries, and a per-alert-id cooldown so a flapping condition
// doesn't spam the endpoint. The caller decides *when* a transition happened.
type WebhookNotifier struct {
	opts        WebhookOptions
	mu          sync.Mutex
	lastFiredAt map[string]int64
}

func NewWebhookNotifier(o WebhookOptions) *WebhookNotifier {
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	if o.Now == nil {
		o.Now = func() int64 { return time.Now().UnixMilli() }
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	return &WebhookNotifier{opts: o, lastFiredAt: map[string]int64{}}
}

// Notify returns true if the POST was accepted (2xx), false if suppressed by cooldown or every
// attempt failed. It blocks for the delivery; the server calls it in a goroutine.
func (n *WebhookNotifier) Notify(a FiringAlert) bool {
	t := n.opts.Now()
	n.mu.Lock()
	prev, seen := n.lastFiredAt[a.ID]
	if seen && t-prev < n.opts.Cooldown.Milliseconds() {
		n.mu.Unlock()
		return false
	}
	n.lastFiredAt[a.ID] = t // hold the cooldown from the moment we start delivering
	n.mu.Unlock()
	return n.deliver(a)
}

func (n *WebhookNotifier) deliver(a FiringAlert) bool {
	body, _ := json.Marshal(map[string]any{"event": "alert.firing", "firedAt": n.opts.Now(), "alert": a})
	for attempt := 0; attempt <= n.opts.Retries; attempt++ {
		if attempt > 0 {
			n.opts.Sleep(n.opts.Backoff * time.Duration(1<<(attempt-1)))
		}
		err := n.attempt(body)
		if err == nil {
			if n.opts.OnSent != nil {
				n.opts.OnSent(a)
			}
			return true
		}
		if attempt == n.opts.Retries {
			if n.opts.OnError != nil {
				n.opts.OnError(err, a)
			}
			return false
		}
	}
	return false
}

func (n *WebhookNotifier) attempt(body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), n.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.opts.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	res, err := n.opts.Client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("webhook responded %d", res.StatusCode)
	}
	return nil
}
