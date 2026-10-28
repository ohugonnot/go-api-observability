package main

import (
	"net/http"
	"time"
)

// demoFaults simulates trouble for the demo and the end-to-end test
// (enabled with DEMO_FAULTS=1, never in a real service):
//
//	?delay=700ms  slow request, inside a "demo.delay" span
//	?fail=1       HTTP 500
//
// It runs after routing, so r.Pattern and the metric labels stay correct.
// maxDemoDelay caps ?delay=; the server WriteTimeout must stay above it.
const maxDemoDelay = 5 * time.Second

func demoFaults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if d, err := time.ParseDuration(q.Get("delay")); err == nil && d > 0 {
			tracer := spanTracer(r)
			_, span := tracer.Start(r.Context(), "demo.delay")
			select {
			case <-time.After(min(d, maxDemoDelay)):
			case <-r.Context().Done():
			}
			span.End()
		}
		if q.Get("fail") == "1" {
			http.Error(w, "simulated failure", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}
