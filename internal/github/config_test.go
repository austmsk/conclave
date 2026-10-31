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
