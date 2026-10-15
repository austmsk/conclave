package llm_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
)

const validYAML = `
models:
  claude-opus-5:
    provider: anthropic
    input_usd_per_mtok: 15
    output_usd_per_mtok: 75
    max_output_tokens: 32000
  gpt-5.6-sol:
    provider: openai
    input_usd_per_mtok: 5
    output_usd_per_mtok: 20
    max_output_tokens: 16000
  qwen3-embedding:
    provider: ollama
    max_output_tokens: 1
roles:
  planner:
    model: claude-opus-5
    fallbacks: [gpt-5.6-sol]
  implementer:
    model: claude-opus-5
embedding:
  model: qwen3-embedding
rate_limit:
  requests_per_minute: 20
  tokens_per_minute: 300000
`

func TestLoadConfigValid(t *testing.T) {
	cfg, err := llm.LoadConfig(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	route := cfg.Roles[contracts.RolePlanner]
	if route.Model != "claude-opus-5" || len(route.Fallbacks) != 1 || route.Fallbacks[0] != "gpt-5.6-sol" {
		t.Fatalf("planner route %+v", route)
	}
	if m := cfg.Models["claude-opus-5"]; m.Provider != "anthropic" || m.OutputUSDPerMTok != 75 {
		t.Fatalf("model %+v", m)
	}
	if cfg.RateLimit.RequestsPerMinute != 20 || cfg.RateLimit.TokensPerMinute != 300000 {
		t.Fatalf("rate limit %+v", cfg.RateLimit)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := []struct {
		name    string
		replace [2]string
	}{
		{"misspelt field", [2]string{"fallbacks:", "fallback:"}},
		{"latest alias", [2]string{"claude-opus-5:\n", "claude-opus-latest:\n"}},
		{"unknown provider", [2]string{"provider: openai", "provider: someone"}},
		{"unknown role", [2]string{"implementer:", "architect:"}},
		{"unconfigured fallback", [2]string{"[gpt-5.6-sol]", "[gpt-9]"}},
		{"duplicate in chain", [2]string{"[gpt-5.6-sol]", "[claude-opus-5]"}},
		{"unconfigured embedding model", [2]string{"model: qwen3-embedding", "model: nomic"}},
		{"negative price", [2]string{"input_usd_per_mtok: 5", "input_usd_per_mtok: -5"}},
		{"zero output limit", [2]string{"max_output_tokens: 16000", "max_output_tokens: 0"}},
		{"negative rate limit", [2]string{"requests_per_minute: 20", "requests_per_minute: -1"}},
		{"negative cache price", [2]string{"input_usd_per_mtok: 15", "input_usd_per_mtok: 15\n    cache_read_usd_per_mtok: -1"}},
		{"empty primary", [2]string{"model: claude-opus-5\n    fallbacks", "model: \"\"\n    fallbacks"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(validYAML, tc.replace[0], tc.replace[1], 1)
			if src == validYAML {
				t.Fatalf("replacement %q did not apply", tc.replace[0])
			}
			if _, err := llm.LoadConfig(strings.NewReader(src)); !errors.Is(err, llm.ErrInvalidConfig) {
				t.Fatalf("got %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestConfigNeedsRoles(t *testing.T) {
	if err := (llm.Config{}).Validate(); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("got %v, want ErrInvalidConfig", err)
	}
}
