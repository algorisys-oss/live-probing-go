package shared

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// HTTPError is a handler failure with a status and an error code ({"error": code}).
type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string { return e.Code }

// Fail returns an *HTTPError.
func Fail(status int, code string) error { return &HTTPError{Status: status, Code: code} }

// Handler is an HTTP handler that may fail; Server maps the error to a response.
type Handler func(w http.ResponseWriter, r *http.Request) error

// Server is a ServeMux wrapped in otelhttp: every request is a server span named by its route
// pattern ("GET /products/{id}") with http.route set, so LiveProbe groups endpoints by route.
type Server struct {
	Name       string
	Mux        *http.ServeMux
	Middleware func(http.Handler) http.Handler // optional, wraps the mux inside the span (e.g. CORS)
}

func NewServer(name string) *Server {
	s := &Server{Name: name, Mux: http.NewServeMux()}
	s.Handle("GET /healthz", func(w http.ResponseWriter, _ *http.Request) error {
		return WriteJSON(w, 200, map[string]string{"status": "ok"})
	})
	return s
}

// Handle registers a route. Fault injection and circuit-breaker sheds map to clean statuses here,
// so a handler only returns the error — no per-site handling.
func (s *Server) Handle(pattern string, h Handler) {
	// otelhttp renames the span from r.Pattern after routing but sets http.route only at span start
	// (before the mux matched), so stamp it here; LiveProbe titles endpoints by method + http.route.
	route := pattern
	if _, path, ok := strings.Cut(pattern, " "); ok {
		route = path
	}
	s.Mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("http.route", route))
		// Finish the work even if the caller gave up (as Node did): otherwise a client timeout
		// cancels this request's downstream calls and paints a *slow* dependency's datastore edges
		// red with instant "context canceled" errors, misattributing the fault. The trace context
		// and values are kept; only cancellation is dropped.
		r = r.WithContext(context.WithoutCancel(r.Context()))
		err := h(w, r)
		if err == nil {
			return
		}
		var fault *InjectedFault
		var httpErr *HTTPError
		switch {
		case errors.As(err, &fault):
			_ = WriteJSON(w, fault.StatusCode, map[string]string{"error": "injected_fault"})
		case IsShed(err):
			_ = WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "circuit_open"})
		case errors.As(err, &httpErr):
			_ = WriteJSON(w, httpErr.Status, map[string]string{"error": httpErr.Code})
		default:
			log.Printf("[%s] %s %s: %v", s.Name, r.Method, r.URL.Path, err)
			_ = WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	})
}

func spanName(_ string, r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.Method + " " + r.URL.Path
}

// Start serves on :port until SIGINT/SIGTERM, then drains and runs onClose.
func (s *Server) Start(port int, onClose func()) {
	var h http.Handler = s.Mux
	if s.Middleware != nil {
		h = s.Middleware(h)
	}
	srv := &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(port)),
		Handler:           otelhttp.NewHandler(h, s.Name, otelhttp.WithSpanNameFormatter(spanName)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("[%s] shutting down", s.Name)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("[%s] listening on :%d", s.Name, port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("[%s] %v", s.Name, err)
	}
	if onClose != nil {
		onClose()
	}
}

// WaitForSignal blocks a worker with no routes of its own until SIGINT/SIGTERM.
func WaitForSignal() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}

func WriteJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

// ReadJSON decodes the request body into v; a malformed body is a 400.
func ReadJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return Fail(http.StatusBadRequest, "invalid_input")
	}
	return nil
}

// NewHTTPClient is an outbound client whose calls are client spans (named "METHOD /path") with
// the traceparent injected, so a request stays one trace across every service boundary.
func NewHTTPClient() *ResilientClient {
	return NewResilientClient(&http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport, otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		})),
		Timeout: 30 * time.Second,
	})
}
