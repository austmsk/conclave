package main

import (
	"os"
	"strings"
	"testing"
	"time"
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
