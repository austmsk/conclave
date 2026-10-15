package llm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/austmsk/conclave/internal/contracts"
)

// Config is the router's role-to-model configuration, loaded from YAML at
// startup. Example:
//
//	models:
//	  claude-opus-5:
//	    provider: anthropic
//	    input_usd_per_mtok: 15
//	    output_usd_per_mtok: 75
//	    max_output_tokens: 32000
//	roles:
//	  planner:
//	    model: claude-opus-5
//	    fallbacks: [gpt-5.6-sol]
//	embedding:
//	  model: qwen3-embedding
type Config struct {
	Models    map[string]ModelConfig       `yaml:"models"`
	Roles     map[contracts.Role]RoleRoute `yaml:"roles"`
	Embedding *EmbeddingRoute              `yaml:"embedding"`
}

// ModelConfig describes one pinned model ID.
type ModelConfig struct {
	Provider         string  `yaml:"provider"`
	InputUSDPerMTok  float64 `yaml:"input_usd_per_mtok"`
	OutputUSDPerMTok float64 `yaml:"output_usd_per_mtok"`
	MaxOutputTokens  int     `yaml:"max_output_tokens"`
}

// RoleRoute is a role's primary model and its ordered fallbacks. Every model
// in the chain passes the tier check when it is reached.
type RoleRoute struct {
	Model     string   `yaml:"model"`
	Fallbacks []string `yaml:"fallbacks"`
}

// EmbeddingRoute names the embedding model. There are no fallbacks: vectors
// from different models are not comparable, so a fallback would silently
// corrupt the index.
type EmbeddingRoute struct {
	Model string `yaml:"model"`
}

// ErrInvalidConfig wraps every configuration validation failure.
var ErrInvalidConfig = errors.New("invalid router configuration")

// knownRoles is every role the router may be configured for.
var knownRoles = []contracts.Role{
	contracts.RoleImplementer,
	contracts.RoleTestWriter,
	contracts.RoleBrief,
	contracts.RolePlanner,
	contracts.RoleJudge,
	contracts.RolePRWriter,
	contracts.RoleDiscussion,
	contracts.RoleConflictResolver,
}

// LoadConfig parses and validates YAML configuration. Unknown fields are an
// error, so a misspelt key fails at startup instead of being ignored.
func LoadConfig(r io.Reader) (Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Config{}, fmt.Errorf("reading router configuration: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%w: parsing YAML: %w", ErrInvalidConfig, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks that every route names a configured, pinned model on a
// known provider. Keys are visited in sorted order so the reported error is
// stable.
func (c Config) Validate() error {
	if len(c.Roles) == 0 {
		return fmt.Errorf("%w: no roles configured", ErrInvalidConfig)
	}
	for _, id := range sortedKeys(c.Models) {
		if err := validateModel(id, c.Models[id]); err != nil {
			return fmt.Errorf("%w: model %q: %w", ErrInvalidConfig, id, err)
		}
	}
	for _, role := range sortedKeys(c.Roles) {
		if err := c.validateRoute(role, c.Roles[role]); err != nil {
			return fmt.Errorf("%w: role %q: %w", ErrInvalidConfig, role, err)
		}
	}
	if c.Embedding != nil {
		if _, ok := c.Models[c.Embedding.Model]; !ok {
			return fmt.Errorf("%w: embedding model %q is not configured", ErrInvalidConfig, c.Embedding.Model)
		}
	}
	return nil
}

func validateModel(id string, m ModelConfig) error {
	if id == "" {
		return errors.New("empty model ID")
	}
	// Aliases invalidate benchmark comparability (ARCHITECTURE.md, Model
	// Routing), so only pinned IDs are accepted.
	if strings.Contains(strings.ToLower(id), "latest") {
		return errors.New("model IDs must be pinned, not a latest alias")
	}
	if !slices.Contains(knownProviders, m.Provider) {
		return fmt.Errorf("unknown provider %q", m.Provider)
	}
	if m.InputUSDPerMTok < 0 || m.OutputUSDPerMTok < 0 {
		return errors.New("prices must not be negative")
	}
	if m.MaxOutputTokens <= 0 {
		return errors.New("max_output_tokens must be positive")
	}
	return nil
}

func (c Config) validateRoute(role contracts.Role, r RoleRoute) error {
	if !slices.Contains(knownRoles, role) {
		return errors.New("unknown role")
	}
	chain := r.chain()
	for i, id := range chain {
		if _, ok := c.Models[id]; !ok {
			return fmt.Errorf("model %q is not configured", id)
		}
		if slices.Contains(chain[:i], id) {
			return fmt.Errorf("model %q appears twice in the chain", id)
		}
	}
	return nil
}

// chain is the primary model followed by its fallbacks.
func (r RoleRoute) chain() []string {
	return append([]string{r.Model}, r.Fallbacks...)
}

func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
