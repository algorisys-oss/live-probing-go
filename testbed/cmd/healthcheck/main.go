// Command healthcheck is the container healthcheck (the distroless image has no shell or curl):
// it GETs the URL given as its argument and exits 0 on a 2xx. Every service starts its HTTP server
// only after its Postgres / Redis / RabbitMQ setup, so a healthy /healthz means "ready for traffic".
package main

import (
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	res, err := (&http.Client{Timeout: 2 * time.Second}).Get(os.Args[1])
	if err != nil || res.StatusCode < 200 || res.StatusCode > 299 {
		os.Exit(1)
	}
}
