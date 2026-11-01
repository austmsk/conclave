package github_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/github"
)

const validConfig = `
app_id: 77
app_slug: conclave
installation_id: 4242
owner_user_id: 1001
bot_user_id: 2002
`

func TestLoadConfig(t *testing.T) {
	// The checked-in file is a placeholder: the server must refuse it
	// until the owner fills in real IDs.
	placeholder, err := os.ReadFile("../../deploy/github.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"valid", validConfig, false},
		{"checked-in placeholder file", string(placeholder), true},
		{"unknown key", validConfig + "owner_login: austmsk\n", true},
		{"zero app_id", strings.Replace(validConfig, "app_id: 77", "app_id: 0", 1), true},
		{"zero installation_id", strings.Replace(validConfig, "installation_id: 4242", "installation_id: 0", 1), true},
		{"zero owner_user_id", strings.Replace(validConfig, "owner_user_id: 1001", "owner_user_id: 0", 1), true},
		{"zero bot_user_id", strings.Replace(validConfig, "bot_user_id: 2002", "bot_user_id: 0", 1), true},
		{"empty app_slug", strings.Replace(validConfig, "app_slug: conclave", `app_slug: ""`, 1), true},
		{"http to a remote host", validConfig + "api_base_url: http://api.example.com\n", true},
		{"https with a query", validConfig + "git_base_url: https://github.com/?x=1\n", true},
		{"http to loopback", validConfig + "api_base_url: http://127.0.0.1:8080\ngit_base_url: http://localhost:9000\n", false},
		{"alerts filled in", validConfig + validAlerts, false},
		{"alerts repository not owner/name", validConfig + strings.Replace(validAlerts, "octo/ops", "ops", 1), true},
		{"alerts repository_id zero", validConfig + strings.Replace(validAlerts, "repository_id: 909", "repository_id: 0", 1), true},
		{"alerts assignee not a login", validConfig + strings.Replace(validAlerts, "assignee: octo", "assignee: \"octo owner\"", 1), true},
		{"alerts unknown key", validConfig + validAlerts + "  labels: [x]\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := github.LoadConfig(strings.NewReader(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("LoadConfig error %v, want error %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, github.ErrInvalidConfig) {
				t.Fatalf("error %v does not wrap ErrInvalidConfig", err)
			}
		})
	}
}

const validAlerts = `alerts:
  repository: octo/ops
  repository_id: 909
  assignee: octo
`

// TestRequireAlerts: the webhook server may leave alerts out, but the
// worker and the CLI, which raise alerts, refuse to start without them.
func TestRequireAlerts(t *testing.T) {
	without, err := github.LoadConfig(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := without.RequireAlerts(); !errors.Is(err, github.ErrInvalidConfig) {
		t.Fatalf("RequireAlerts without alerts: %v, want ErrInvalidConfig", err)
	}
	with, err := github.LoadConfig(strings.NewReader(validConfig + validAlerts))
	if err != nil {
		t.Fatal(err)
	}
	if err := with.RequireAlerts(); err != nil {
		t.Fatalf("RequireAlerts with alerts: %v", err)
	}
	if r := with.AlertsRepo(); r.ID != 909 || r.FullName != "octo/ops" || r.InstallationID != 4242 {
		t.Fatalf("AlertsRepo = %+v", r)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := github.LoadConfig(strings.NewReader(validConfig + "trigger_label: \"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TriggerLabel != github.DefaultTriggerLabel || cfg.APIBaseURL != github.DefaultAPIBaseURL || cfg.GitBaseURL != github.DefaultGitBaseURL {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

// TestBotIdentity: commits are authored as the App's bot, with the noreply
// address GitHub links to the bot's user ID (FR-S17).
func TestBotIdentity(t *testing.T) {
	cfg, err := github.LoadConfig(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	name, email := cfg.BotIdentity()
	if name != "conclave[bot]" || email != "2002+conclave[bot]@users.noreply.github.com" {
		t.Fatalf("BotIdentity = %q <%s>", name, email)
	}
}
