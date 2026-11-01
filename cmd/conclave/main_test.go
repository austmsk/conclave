package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/alert"
)

// TestReadSecretsNamesMissing names a missing secret, never a value, and
// unsets each one it read.
func TestReadSecretsNamesMissing(t *testing.T) {
	t.Setenv(databaseURLEnv, "postgres://canary_secret@localhost/x")
	t.Setenv("CONCLAVE_TEST_MISSING", "")
	_, err := readSecrets(databaseURLEnv, "CONCLAVE_TEST_MISSING")
	if err == nil || !strings.Contains(err.Error(), "CONCLAVE_TEST_MISSING") || strings.Contains(err.Error(), "canary_secret") {
		t.Fatalf("error %v, want the missing name and no value", err)
	}
	t.Setenv(databaseURLEnv, "postgres://canary_secret@localhost/x")
	got, err := readSecrets(databaseURLEnv)
	if err != nil || got[databaseURLEnv] == "" {
		t.Fatalf("readSecrets = %v, %v", got, err)
	}
	if v, set := lookupEnv(databaseURLEnv); set {
		t.Fatalf("%s still set to %q", databaseURLEnv, v)
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"halt", []string{"halt", "-reason", "drill", "-alert-log", "/tmp/a"}, false},
		{"halt without reason", []string{"halt", "-alert-log", "/tmp/a"}, true},
		{"halt without log", []string{"halt", "-reason", "drill"}, true},
		{"resume keep", []string{"resume", "-reason", "ok", "-keep"}, false},
		{"resume without reason", []string{"resume"}, true},
		{"alert-test", []string{"alert-test", "-alert-log", "/tmp/a"}, false},
		{"report", []string{"report", "-from", "2026-10-01T00:00:00Z"}, false},
		{"unknown flag", []string{"report", "-keep"}, true},
		{"unknown command", []string{"explode"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseFlags(tt.args[0], tt.args[1:])
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFlags(%v) = %v, want error %t", tt.args, err, tt.wantErr)
			}
		})
	}
}

func TestReportWindow(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	from, to, err := reportWindow("", "", now)
	if err != nil || !from.IsZero() || !to.Equal(now) {
		t.Fatalf("defaults = %s, %s, %v", from, to, err)
	}
	if _, _, err := reportWindow("2026-11-01T00:00:00Z", "2026-10-01T00:00:00Z", now); err == nil {
		t.Fatal("a window ending before it starts was accepted")
	}
	if _, _, err := reportWindow("yesterday", "", now); err == nil {
		t.Fatal("a bad -from was accepted")
	}
}

func lookupEnv(name string) (string, bool) { return os.LookupEnv(name) }

// TestHaltConnectorFallsBack builds the halt's connections with GitHub's
// configuration missing and Temporal and Docker unreachable: each step
// fails with its reason, and the halt alert still reaches the local log.
func TestHaltConnectorFallsBack(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	logPath := filepath.Join(t.TempDir(), "alerts.jsonl")
	o := options{githubConfig: "testdata/missing.yaml", alertLog: logPath, temporalAddr: "127.0.0.1:1", namespace: "default"}
	var out bytes.Buffer
	k := newKillSwitch(o, nil, &out)
	h := &haltConnector{o: o, keyFile: "missing.pem", log: slog.New(slog.NewTextHandler(io.Discard, nil)), timeout: time.Second}
	defer h.close()
	h.connect(t.Context(), k)

	if err := k.raise(t.Context(), alert.Halt(1, "drill", time.Now())); err == nil {
		t.Error("raise reported delivery with GitHub unavailable")
	}
	raw, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(raw), `"key":"halt:1"`) || !strings.Contains(string(raw), `"outcome":"failed"`) {
		t.Fatalf("alert log %q (%v), want the halt alert and its failed delivery", raw, err)
	}
	if _, err := k.runs.open(t.Context()); err == nil {
		t.Error("listing workflows succeeded with Temporal unreachable")
	}
	if _, err := k.sweep(t.Context()); err == nil {
		t.Error("sweep succeeded with Docker unreachable")
	}
	for _, want := range []string{"no workflow is cancelled", "no sandbox is swept"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
