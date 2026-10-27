package shared

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// Client-side resilience: per-attempt timeout, bounded retries with backoff, and a per-host
// circuit breaker. Paired with fault injection on the server side it makes the classic failure
// story visible in LiveProbe — retries show as repeated outbound spans (the ×N fold), a tripped
// breaker sheds load off a sick dependency. Every knob defaults to off, so an unconfigured
// Policy is exactly one plain request.

type Policy struct {
	TimeoutMs        int // per-attempt; 0 = none
	Retries          int // extra attempts after the first
	BackoffMs        int // base backoff, doubled each retry
	BreakerThreshold int // consecutive failures that open the breaker; 0 = off
	BreakerResetMs   int // how long it stays open before a half-open trial
}

var NoResilience = Policy{}

// PolicyFromEnv reads ${prefix}_TIMEOUT_MS / _RETRIES / _BACKOFF_MS / _BREAKER_THRESHOLD /
// _BREAKER_RESET_MS; anything unset leaves that knob off.
func PolicyFromEnv(prefix string) Policy {
	return Policy{
		TimeoutMs:        EnvInt(prefix+"_TIMEOUT_MS", 0),
		Retries:          EnvInt(prefix+"_RETRIES", 0),
		BackoffMs:        EnvInt(prefix+"_BACKOFF_MS", 100),
		BreakerThreshold: EnvInt(prefix+"_BREAKER_THRESHOLD", 0),
		BreakerResetMs:   EnvInt(prefix+"_BREAKER_RESET_MS", 10_000),
	}
}

// CircuitOpenError: the breaker is open and the call was shed without touching the network.
// The HTTP layer maps it to 503 so it surfaces as a retryable failure.
type CircuitOpenError struct{ Host string }

func (e *CircuitOpenError) Error() string { return "circuit open for " + e.Host }

type breaker struct {
	failures int
	openedAt time.Time // zero when closed
}

// ResilientClient wraps an http.Client with the policy logic. One breaker per host across the
// process: a sick dependency trips once for every caller in this service, which is the point.
type ResilientClient struct {
	HTTP  *http.Client
	Now   func() time.Time
	Sleep func(context.Context, time.Duration)

	mu       sync.Mutex
	breakers map[string]*breaker
}

func NewResilientClient(c *http.Client) *ResilientClient {
	return &ResilientClient{HTTP: c, Now: time.Now, Sleep: sleepCtx, breakers: map[string]*breaker{}}
}

// Do sends req under policy p. It retries a transport error, a timeout or a 5xx (never a 4xx),
// returns the last response even if it is a 5xx (so the caller can read the body), and errors
// only on a transport failure that never produced a response, or *CircuitOpenError.
// The request body, if any, must be rewindable (http.NewRequest with a bytes/strings reader).
func (c *ResilientClient) Do(req *http.Request, p Policy) (*http.Response, error) {
	host := req.URL.Host
	breakerOn := p.BreakerThreshold > 0
	c.mu.Lock()
	state := c.breakers[host]
	if state == nil {
		state = &breaker{}
		c.breakers[host] = state
	}
	if breakerOn && !state.openedAt.IsZero() && c.Now().Sub(state.openedAt) < time.Duration(p.BreakerResetMs)*time.Millisecond {
		c.mu.Unlock()
		return nil, &CircuitOpenError{Host: host}
	}
	c.mu.Unlock()

	var lastErr error
	var lastRes *http.Response
	for i := 0; i <= p.Retries; i++ {
		if i > 0 {
			c.Sleep(req.Context(), time.Duration(p.BackoffMs)*time.Millisecond*time.Duration(1<<(i-1)))
		}
		res, err := c.attempt(req, p.TimeoutMs)
		if err != nil {
			lastErr, lastRes = err, nil
			continue
		}
		if res.StatusCode >= 500 {
			if lastRes != nil {
				lastRes.Body.Close()
			}
			lastRes, lastErr = res, nil
			continue // retryable server error
		}
		if lastRes != nil {
			lastRes.Body.Close()
		}
		if breakerOn {
			c.mu.Lock()
			state.failures, state.openedAt = 0, time.Time{}
			c.mu.Unlock()
		}
		return res, nil
	}

	// Retries exhausted: a 5xx counts as a breaker failure but is a real response we hand back.
	if breakerOn {
		c.mu.Lock()
		state.failures++
		if state.failures >= p.BreakerThreshold {
			state.openedAt = c.Now()
		}
		c.mu.Unlock()
	}
	if lastRes != nil {
		return lastRes, nil
	}
	return nil, lastErr
}

func (c *ResilientClient) attempt(req *http.Request, timeoutMs int) (*http.Response, error) {
	r := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		r.Body = body
	}
	if timeoutMs <= 0 {
		return c.HTTP.Do(r)
	}
	ctx, cancel := context.WithTimeout(req.Context(), time.Duration(timeoutMs)*time.Millisecond)
	res, err := c.HTTP.Do(r.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	// Keep the timeout context alive until the body is read; cancel when it is closed.
	res.Body = &cancelOnClose{ReadCloser: res.Body, cancel: cancel}
	return res, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// IsShed reports whether err is a breaker shed.
func IsShed(err error) bool {
	var open *CircuitOpenError
	return errors.As(err, &open)
}
