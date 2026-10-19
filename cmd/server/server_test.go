package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewServerBoundsEveryPhase(t *testing.T) {
	srv := newServer("127.0.0.1:0", http.NotFoundHandler())
	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 5 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 10 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 10 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("%s is %s, want %s", tt.name, tt.got, tt.want)
			}
		})
	}
	if srv.MaxHeaderBytes != 64<<10 {
		t.Fatalf("MaxHeaderBytes is %d, want %d", srv.MaxHeaderBytes, 64<<10)
	}
}

func TestMuxRoutes(t *testing.T) {
	webhook := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	mux := newMux(webhook)
	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/webhooks/github", http.StatusAccepted},
		{http.MethodGet, "/webhooks/github", http.StatusMethodNotAllowed},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d", rec.Code, tt.want)
			}
		})
	}
}
