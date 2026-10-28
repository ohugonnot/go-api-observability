// Command loadgen sends a mix of normal, slow and failing requests to the
// invoice API (started with DEMO_FAULTS=1), to exercise tail sampling.
package main

import (
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type kind int

const (
	normal kind = iota
	slow
	failing
)

func main() {
	target := flag.String("url", "http://127.0.0.1:8080", "API base URL")
	n := flag.Int("n", 1000, "number of requests")
	workers := flag.Int("c", 8, "concurrent workers (>= 1)")
	errRate := flag.Float64("errors", 0.05, "share of requests answered with a 500 (?fail=1)")
	slowRate := flag.Float64("slow", 0.05, "share of slow requests (?delay=700ms)")
	flag.Parse()

	base, err := url.Parse(*target)
	if err != nil || base.Scheme == "" || base.Host == "" {
		fatalf("invalid -url %q", *target)
	}
	if *workers < 1 || *n < 0 || *errRate < 0 || *slowRate < 0 || *errRate+*slowRate > 1 {
		fatalf("need -c >= 1, -n >= 0 and 0 <= -errors + -slow <= 1")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	jobs := make(chan kind)
	var (
		mu       sync.Mutex
		byStatus = map[int]int{}
		sent     = map[kind]int{}
		failures int
		wg       sync.WaitGroup
	)
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range jobs {
				status, err := send(client, base.String(), k)
				mu.Lock()
				if err != nil {
					failures++
				} else {
					byStatus[status]++
				}
				mu.Unlock()
			}
		}()
	}
	start := time.Now()
	for range *n {
		k := normal
		switch p := rand.Float64(); {
		case p < *errRate:
			k = failing
		case p < *errRate+*slowRate:
			k = slow
		}
		sent[k]++
		jobs <- k
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("loadgen: %d requests in %s, by status: %v, transport errors: %d\n",
		*n, time.Since(start).Round(time.Millisecond), byStatus, failures)
	fmt.Printf("sent: normal=%d slow=%d failing=%d\n", sent[normal], sent[slow], sent[failing])

	if failures > 0 {
		os.Exit(1)
	}
	// The API should fail about as often as asked: otherwise DEMO_FAULTS is
	// off, or something else is broken.
	var errors5xx int
	for code, c := range byStatus {
		if code >= 500 {
			errors5xx += c
		}
	}
	if *n > 0 {
		share := float64(errors5xx) / float64(*n)
		if (*errRate > 0 && errors5xx == 0) || share > 2**errRate+0.01 {
			fatalf("5xx share %.1f%% is far from -errors %.1f%%", share*100, *errRate*100)
		}
	}
}

func send(c *http.Client, base string, k kind) (int, error) {
	var (
		req *http.Request
		err error
	)
	switch {
	case k == failing:
		req, err = http.NewRequest("GET", base+"/invoices/1?fail=1", nil)
	case k == slow:
		req, err = http.NewRequest("GET", base+"/invoices/1?delay=700ms", nil)
	case rand.Float64() < 0.83:
		// IDs 1..20: most do not exist (404), which is normal traffic too.
		req, err = http.NewRequest("GET", fmt.Sprintf("%s/invoices/%d", base, 1+rand.IntN(20)), nil)
	default:
		body := `{"customer":"Demo","email":"demo@example.com","amount_cents":1000,"currency":"EUR"}`
		req, err = http.NewRequest("POST", base+"/invoices", strings.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection is reused
	return resp.StatusCode, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "loadgen: "+format+"\n", args...)
	os.Exit(1)
}
