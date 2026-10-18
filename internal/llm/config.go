package llm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
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
//	    cache_write_usd_per_mtok: 18.75
//	    cache_read_usd_per_mtok: 1.5
//	    max_output_tokens: 32000
//	roles:
//	  planner:
//	    model: claude-opus-5
//	    fallbacks: [gpt-5.6-sol]
//	embedding:
//	  model: qwen3-embedding
//	rate_limit:
//	  requests_per_minute: 20
//	  tokens_per_minute: 300000
//	budgets:
//	  feature_request_tokens: 5000000
//	  monthly_usd: 200
//
// Prices are per million tokens and must be copied from the provider's
// current price list.
type Config struct {
	Models    map[string]ModelConfig       `yaml:"models"`
	Roles     map[contracts.Role]RoleRoute `yaml:"roles"`
	Embedding *EmbeddingRoute              `yaml:"embedding"`
	RateLimit RateLimit                    `yaml:"rate_limit"`
	Budgets   Budgets                      `yaml:"budgets"`
}

// Budgets are the owner's platform caps (FR-SEC9). Each attempt's own token
// budget is set per scope; these bound everything above it. Neither has a
// default: a missing cap is a configuration error, never an unlimited one.
type Budgets struct {
	// FeatureRequestTokens caps input plus output tokens over every call
	// made for one feature request, in all its attempts and outside them.
	FeatureRequestTokens int `yaml:"feature_request_tokens"`
	// MonthlyUSD caps spend across all repositories in a UTC calendar
	// month.
	MonthlyUSD float64 `yaml:"monthly_usd"`
}

// validate requires both caps. A placeholder (-1) or a missing key (0)
// fails here with the key named, so the owner knows what to set.
func (b Budgets) validate() error {
	if b.FeatureRequestTokens <= 0 {
		return fmt.Errorf("%w: budgets.feature_request_tokens is %d: set the per-feature-request token cap to a positive number",
			ErrInvalidConfig, b.FeatureRequestTokens)
	}
	// Written as a negation so that NaN fails too.
	if !(b.MonthlyUSD > 0) {
		return fmt.Errorf("%w: budgets.monthly_usd is %v: set the monthly spending cap to a positive number of US dollars",
			ErrInvalidConfig, b.MonthlyUSD)
	}
	return nil
}

// RateLimit caps how fast one router, and so one worker process, sends
// work to providers. A zero field takes its default.
type RateLimit struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
	TokensPerMinute   int `yaml:"tokens_per_minute"`
}

// withDefaults fills unset fields.
func (r RateLimit) withDefaults() RateLimit {
	if r.RequestsPerMinute == 0 {
		r.RequestsPerMinute = DefaultRequestsPerMinute
	}
	if r.TokensPerMinute == 0 {
		r.TokensPerMinute = DefaultTokensPerMinute
	}
	return r
}

// ModelConfig describes one pinned model ID. Cache prices left at zero are
// charged at the input price.
type ModelConfig struct {
	Provider             string  `yaml:"provider"`
	InputUSDPerMTok      float64 `yaml:"input_usd_per_mtok"`
	OutputUSDPerMTok     float64 `yaml:"output_usd_per_mtok"`
	CacheWriteUSDPerMTok float64 `yaml:"cache_write_usd_per_mtok"`
	CacheReadUSDPerMTok  float64 `yaml:"cache_read_usd_per_mtok"`
	MaxOutputTokens      int     `yaml:"max_output_tokens"`
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
	var cfg Config
	if err := decodeStrict(r, &cfg); err != nil {
		return Config{}, fmt.Errorf("router configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// decodeStrict decodes YAML into v, rejecting unknown keys so a misspelt
// setting stops startup instead of being ignored. Every configuration file
// goes through it.
func decodeStrict(r io.Reader, v any) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: parsing YAML: %w", ErrInvalidConfig, err)
	}
	return nil
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
	if c.RateLimit.RequestsPerMinute < 0 || c.RateLimit.TokensPerMinute < 0 {
		return fmt.Errorf("%w: rate limits must not be negative", ErrInvalidConfig)
	}
	if err := c.Budgets.validate(); err != nil {
		return err
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
	for _, price := range []float64{m.InputUSDPerMTok, m.OutputUSDPerMTok, m.CacheWriteUSDPerMTok, m.CacheReadUSDPerMTok} {
		// NaN compares false with everything, so a NaN price would
		// pass a sign check and make every monthly-cap comparison false.
		if math.IsNaN(price) || math.IsInf(price, 0) {
			return errors.New("prices must be finite numbers")
		}
		if price < 0 {
			return errors.New("prices must not be negative")
		}
	}
	// A hosted model priced at zero would record every call as free, and
	// a USD cap summing cost_events would never fire. Only models on the
	// owner's machines may be free. Cache prices may stay zero: they fall
	// back to the input price.
	if !slices.Contains(localProviders, m.Provider) && (m.InputUSDPerMTok == 0 || m.OutputUSDPerMTok == 0) {
		return fmt.Errorf("provider %q is hosted, so input and output prices must be positive", m.Provider)
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

// clone copies every map and slice, so a caller that keeps and later
// changes its Config cannot undo what Validate checked.
func (c Config) clone() Config {
	out := c
	out.Models = maps.Clone(c.Models)
	out.Roles = make(map[contracts.Role]RoleRoute, len(c.Roles))
	for role, r := range c.Roles {
		r.Fallbacks = slices.Clone(r.Fallbacks)
		out.Roles[role] = r
	}
	if c.Embedding != nil {
		e := *c.Embedding
		out.Embedding = &e
	}
	return out
}

func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
