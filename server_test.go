package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatusCodes(t *testing.T) {
	app := newTestApp(t)
	valid := `{"customer":"Bob","email":"bob@example.com","amount_cents":500,"currency":"EUR"}`
	tests := []struct {
		name, method, target, body string
		want                       int
	}{
		{"health", "GET", "/healthz", "", http.StatusOK},
		{"found", "GET", "/invoices/1", "", http.StatusOK},
		{"missing", "GET", "/invoices/999", "", http.StatusNotFound},
		{"non numeric id", "GET", "/invoices/abc", "", http.StatusNotFound},
		{"id too long", "GET", "/invoices/" + strings.Repeat("9", 19), "", http.StatusNotFound},
		{"create", "POST", "/invoices", valid, http.StatusCreated},
		{"bad json", "POST", "/invoices", `{not json`, http.StatusBadRequest},
		{"trailing data", "POST", "/invoices", valid + `{"x":1}`, http.StatusBadRequest},
		{"unknown field", "POST", "/invoices", `{"customer":"Bob","admin":true}`, http.StatusBadRequest},
		{"body too large", "POST", "/invoices", `{"customer":"` + strings.Repeat("a", 1<<20) + `"}`, http.StatusRequestEntityTooLarge},
		{"zero amount", "POST", "/invoices", `{"customer":"Bob","amount_cents":0,"currency":"EUR"}`, http.StatusUnprocessableEntity},
		{"blank customer", "POST", "/invoices", `{"customer":"  ","amount_cents":1,"currency":"EUR"}`, http.StatusUnprocessableEntity},
		{"customer 256 bytes", "POST", "/invoices", `{"customer":"` + strings.Repeat("é", 128) + `","amount_cents":1,"currency":"EUR"}`, http.StatusCreated},
		{"customer 257 bytes", "POST", "/invoices", `{"customer":"` + strings.Repeat("a", 257) + `","amount_cents":1,"currency":"EUR"}`, http.StatusUnprocessableEntity},
		{"email 256 bytes", "POST", "/invoices", `{"customer":"Bob","email":"` + strings.Repeat("a", 256) + `","amount_cents":1,"currency":"EUR"}`, http.StatusCreated},
		{"email 257 bytes", "POST", "/invoices", `{"customer":"Bob","email":"` + strings.Repeat("a", 257) + `","amount_cents":1,"currency":"EUR"}`, http.StatusUnprocessableEntity},
		{"too large after the JSON", "POST", "/invoices", valid + strings.Repeat(" ", 1<<20), http.StatusRequestEntityTooLarge},
		{"lowercase currency", "POST", "/invoices", `{"customer":"Bob","amount_cents":1,"currency":"eur"}`, http.StatusUnprocessableEntity},
		{"non ascii currency", "POST", "/invoices", `{"customer":"Bob","amount_cents":1,"currency":"€"}`, http.StatusUnprocessableEntity},
		{"method not allowed", "DELETE", "/invoices/1", "", http.StatusMethodNotAllowed},
		{"no route", "GET", "/nope", "", http.StatusNotFound},
		{"metrics not public", "GET", "/metrics", "", http.StatusNotFound},
		{"demo failure", "GET", "/invoices/1?fail=1", "", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := app.do(tt.method, tt.target, tt.body).Code; got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDemoFaultsOffByDefault(t *testing.T) {
	app := newTestAppWith(t, false)
	if got := app.do("GET", "/invoices/1?fail=1", "").Code; got != http.StatusOK {
		t.Errorf("with faults off, ?fail=1 status = %d, want 200", got)
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := config{Addr: ":8080", MetricsAddr: "127.0.0.1:9091", SampleRatio: 1}
	if cfg != want {
		t.Errorf("defaults = %+v, want %+v (metrics must stay on loopback)", cfg, want)
	}
	if _, err := loadConfig(func(k string) string {
		return map[string]string{"TRACE_SAMPLE_RATIO": "2"}[k]
	}); err == nil {
		t.Error("an invalid TRACE_SAMPLE_RATIO must be rejected")
	}
}

func TestServerTimeouts(t *testing.T) {
	srv := newServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 || srv.MaxHeaderBytes != 16<<10 {
		t.Errorf("missing timeouts or header limit: %+v", srv)
	}
	if srv.ReadTimeout < srv.ReadHeaderTimeout {
		t.Errorf("ReadTimeout %s < ReadHeaderTimeout %s", srv.ReadTimeout, srv.ReadHeaderTimeout)
	}
	if srv.WriteTimeout <= maxDemoDelay || srv.WriteTimeout <= srv.ReadTimeout {
		t.Errorf("WriteTimeout %s must exceed the %s demo delay and ReadTimeout %s",
			srv.WriteTimeout, maxDemoDelay, srv.ReadTimeout)
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ok.Close()
	_, port, _ := net.SplitHostPort(ok.Listener.Addr().String())
	if err := healthcheck(":" + port); err != nil {
		t.Errorf("healthy server: %v", err)
	}
	ok.Close()
	if err := healthcheck(":" + port); err == nil {
		t.Error("stopped server reported healthy")
	}

	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer sick.Close()
	_, port, _ = net.SplitHostPort(sick.Listener.Addr().String())
	if err := healthcheck(":" + port); err == nil {
		t.Error("a 503 /healthz reported healthy")
	}
}

// 18 digits reach the store (and its span), 19 are rejected before it.
func TestIDLengthBoundary(t *testing.T) {
	app := newTestApp(t)
	storeCalls := func() int {
		n := 0
		for _, s := range app.spans.Ended() {
			if s.Name() == "store.Get" {
				n++
			}
		}
		return n
	}
	app.do("GET", "/invoices/"+strings.Repeat("9", 18), "")
	if storeCalls() != 1 {
		t.Fatalf("18-digit id: store.Get calls = %d, want 1", storeCalls())
	}
	app.do("GET", "/invoices/"+strings.Repeat("9", 19), "")
	if storeCalls() != 1 {
		t.Errorf("19-digit id reached store.Get")
	}
}

// Invalid IDs are rejected before the store: no span attribute, no log of the raw value.
func TestInvalidIDLeavesNoTrace(t *testing.T) {
	app := newTestApp(t)
	raw := "x" + strings.Repeat("A", 5000)
	app.do("GET", "/invoices/"+raw, "")
	for _, s := range app.spans.Ended() {
		if s.Name() == "store.Get" {
			t.Error("store.Get called with an invalid id")
		}
	}
	if strings.Contains(app.logs.String(), "AAAA") {
		t.Error("raw id logged")
	}
}
