// Command loadgen sends a mix of normal, slow and failing requests to the
// invoice API (started with DEMO_FAULTS=1), to exercise tail sampling.
package main

import (
	"errors"
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
	if err := checkFlags(*n, *workers, *errRate, *slowRate); err != nil {
		fatalf("%v", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	jobs := make(chan string)
	var (
		mu         sync.Mutex
		byStatus   = map[int]int{}
		sent       = map[string]int{}
		transports int
		wg         sync.WaitGroup
	)
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for kind := range jobs {
				status, err := send(client, base.String(), kind)
				mu.Lock()
				if err != nil {
					transports++
				} else {
					byStatus[status]++
				}
				mu.Unlock()
			}
		}()
	}
	start := time.Now()
	for range *n {
		kind := "normal"
		switch p := rand.Float64(); {
		case p < *errRate:
			kind = "failing"
		case p < *errRate+*slowRate:
			kind = "slow"
		}
		sent[kind]++
		jobs <- kind
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("loadgen: %d requests in %s, by status: %v, transport errors: %d\n",
		*n, time.Since(start).Round(time.Millisecond), byStatus, transports)
	fmt.Printf("sent: normal=%d slow=%d failing=%d\n", sent["normal"], sent["slow"], sent["failing"])
	if err := verdict(byStatus, sent, transports); err != nil {
		fatalf("%v", err)
	}
}

// checkFlags is written in the positive (x >= 0 && x <= 1) so NaN fails it.
func checkFlags(n, workers int, errRate, slowRate float64) error {
	ok := func(x float64) bool { return x >= 0 && x <= 1 }
	if n < 0 || workers < 1 || !ok(errRate) || !ok(slowRate) || !ok(errRate+slowRate) {
		return errors.New("need -n >= 0, -c >= 1 and 0 <= -errors, -slow, their sum <= 1")
	}
	return nil
}

// verdict checks the API answered as asked: one 5xx per failing request sent
// (with DEMO_FAULTS off, ?fail=1 is ignored and there would be none), and
// few others. It compares with what was actually sent, not with the rate.
func verdict(byStatus map[int]int, sent map[string]int, transportErrs int) error {
	if transportErrs > 0 {
		return fmt.Errorf("%d transport errors", transportErrs)
	}
	var errs5xx int
	for code, c := range byStatus {
		if code >= 500 {
			errs5xx += c
		}
	}
	failing := sent["failing"]
	if failing == 0 {
		return nil
	}
	if errs5xx < failing {
		return fmt.Errorf("%d 5xx for %d failing requests sent: is DEMO_FAULTS=1 set?", errs5xx, failing)
	}
	if extra := errs5xx - failing; extra > max(2, failing/10) {
		return fmt.Errorf("%d unexpected 5xx beyond the %d failing requests sent", extra, failing)
	}
	return nil
}

func send(c *http.Client, base, kind string) (int, error) {
	var (
		req *http.Request
		err error
	)
	switch {
	case kind == "failing":
		req, err = http.NewRequest("GET", base+"/invoices/1?fail=1", nil)
	case kind == "slow":
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
