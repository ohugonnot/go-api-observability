// Command loadgen sends a mix of normal, slow and failing requests to the
// invoice API (started with DEMO_FAULTS=1), to exercise tail sampling.
package main

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	target := flag.String("url", "http://127.0.0.1:8080", "API base URL")
	n := flag.Int("n", 1000, "number of requests")
	workers := flag.Int("c", 8, "concurrent workers")
	errRate := flag.Float64("errors", 0.05, "share of requests answered with a 500 (?fail=1)")
	slowRate := flag.Float64("slow", 0.05, "share of slow requests (?delay=700ms)")
	flag.Parse()

	client := &http.Client{Timeout: 10 * time.Second}
	jobs := make(chan int)
	var (
		mu       sync.Mutex
		byStatus = map[int]int{}
		failures int
		wg       sync.WaitGroup
	)
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				status, err := send(client, *target, *errRate, *slowRate)
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
	for i := range *n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("loadgen: %d requests in %s, by status: %v, transport errors: %d\n",
		*n, time.Since(start).Round(time.Millisecond), byStatus, failures)
	if failures > 0 {
		os.Exit(1)
	}
}

func send(c *http.Client, base string, errRate, slowRate float64) (int, error) {
	var req *http.Request
	switch p := rand.Float64(); {
	case p < errRate:
		req, _ = http.NewRequest("GET", base+"/invoices/1?fail=1", nil)
	case p < errRate+slowRate:
		req, _ = http.NewRequest("GET", base+"/invoices/1?delay=700ms", nil)
	case p < 0.85:
		// IDs 1..20: most do not exist (404), which is normal traffic too.
		req, _ = http.NewRequest("GET", fmt.Sprintf("%s/invoices/%d", base, 1+rand.IntN(20)), nil)
	default:
		body := `{"customer":"Demo","email":"demo@example.com","amount_cents":1000,"currency":"EUR"}`
		req, _ = http.NewRequest("POST", base+"/invoices", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
