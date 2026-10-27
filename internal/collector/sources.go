package collector

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"liveprobe/internal/core"
)

// --- stdin ---------------------------------------------------------------------------------

// ReadNDJSON reads newline-delimited JSON until EOF; each line is one raw source event.
// Blank and malformed lines are skipped.
func ReadNDJSON(r io.Reader, onEvent func(any)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var v any
		if json.Unmarshal(line, &v) == nil {
			onEvent(v)
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("[collector] stdin read stopped: %v", err) // e.g. a line over 16 MB
	}
}

// --- http ----------------------------------------------------------------------------------

const maxHTTPEventBytes = 5 * 1024 * 1024 // one event per POST; anything bigger is a mistake

// HTTPSourceHandler accepts POSTed JSON — one raw event or an array — on any path. Path-agnostic
// on purpose: emitters that push to per-type endpoints (POST /v1/event/instrumentation|log|audit)
// can be repointed here unchanged; the event type lives in the payload.
func HTTPSourceHandler(onEvent func(any)) http.Handler {
	reply := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.RequestURI() == "/healthz" {
			w.Header().Set("content-type", "text/plain")
			_, _ = io.WriteString(w, "ok")
			return
		}
		if r.Method != http.MethodPost {
			reply(w, 405, `{"error":"POST JSON events"}`)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHTTPEventBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				reply(w, 413, `{"error":"body too large"}`)
			}
			return // client hung up mid-body otherwise; nothing to do
		}
		var v any
		if json.Unmarshal(body, &v) != nil {
			reply(w, 400, `{"error":"invalid json"}`)
			return
		}
		if arr, ok := v.([]any); ok {
			for _, item := range arr {
				onEvent(item)
			}
		} else {
			onEvent(v)
		}
		reply(w, 202, `{"ok":true}`)
	})
}

// --- sink ----------------------------------------------------------------------------------

// Sink buffers normalized events and POSTs them to LiveProbe's native ingest in batches: when
// the buffer reaches batchSize, and every flushEvery.
type Sink struct {
	endpoint  string
	batchSize int
	client    *http.Client

	mu     sync.Mutex
	buffer []core.Event

	inflight sync.WaitGroup
	stop     chan struct{}
	done     chan struct{}
}

func NewSink(baseURL string, batchSize int, flushEvery time.Duration) *Sink {
	s := &Sink{
		endpoint:  strings.TrimRight(baseURL, "/") + "/v1/events",
		batchSize: batchSize,
		client:    &http.Client{Timeout: 30 * time.Second},
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go func() {
		defer close(s.done)
		t := time.NewTicker(flushEvery)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.Flush()
			}
		}
	}()
	return s
}

func (s *Sink) Push(events []core.Event) {
	if len(events) == 0 {
		return
	}
	s.mu.Lock()
	s.buffer = append(s.buffer, events...)
	var batch []core.Event
	if len(s.buffer) >= s.batchSize {
		batch, s.buffer = s.buffer, nil // cut the batch now; only the POST is asynchronous
	}
	s.mu.Unlock()
	if batch != nil {
		s.inflight.Add(1)
		go func() {
			defer s.inflight.Done()
			s.post(batch)
		}()
	}
}

// Flush posts whatever is buffered.
func (s *Sink) Flush() {
	s.mu.Lock()
	batch := s.buffer
	s.buffer = nil
	s.mu.Unlock()
	s.post(batch)
}

// post delivers one batch. Failures are logged, never returned: one bad batch must not stop the
// stream.
func (s *Sink) post(batch []core.Event) {
	if len(batch) == 0 {
		return
	}
	body, err := json.Marshal(map[string]any{"events": batch})
	if err != nil {
		log.Printf("[collector] encode failed: %v", err)
		return
	}
	res, err := s.client.Post(s.endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[collector] ingest failed: %v", err)
		return
	}
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		log.Printf("[collector] ingest returned %d", res.StatusCode)
	}
}

// Close stops the timer, waits for in-flight batches and flushes the remainder.
func (s *Sink) Close() {
	close(s.stop)
	<-s.done
	s.inflight.Wait()
	s.Flush()
}
