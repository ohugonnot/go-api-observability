package main

import (
	"net/http"
	"testing"
)

func TestStatusCodes(t *testing.T) {
	app := newTestApp(t)
	tests := []struct {
		method, target, body string
		want                 int
	}{
		{"GET", "/healthz", "", http.StatusOK},
		{"GET", "/invoices/1", "", http.StatusOK},
		{"GET", "/invoices/999", "", http.StatusNotFound},
		{"POST", "/invoices", `{"customer":"Bob","email":"bob@example.com","amount_cents":500,"currency":"EUR"}`, http.StatusCreated},
		{"POST", "/invoices", `{not json`, http.StatusBadRequest},
		{"POST", "/invoices", `{"customer":"Bob","amount_cents":0,"currency":"EUR"}`, http.StatusUnprocessableEntity},
		{"DELETE", "/invoices/1", "", http.StatusMethodNotAllowed},
		{"GET", "/nope", "", http.StatusNotFound},
		{"GET", "/invoices/1?fail=1", "", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			if got := app.do(tt.method, tt.target, tt.body).Code; got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
}
