package github

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	// DefaultAPIBaseURL is GitHub's REST endpoint.
	DefaultAPIBaseURL = "https://api.github.com"
	// DefaultGitBaseURL is where repositories are cloned from and pushed to.
	DefaultGitBaseURL = "https://github.com"
	// DefaultTriggerLabel is the label whose application by the owner
	// starts a run (FR-S1).
	DefaultTriggerLabel = "agent"

	// WebhookSecretEnv names the variable the server reads the webhook
	// secret from. The owner's secrets file outside the repository sets it,
	// and the server unsets it once read.
	WebhookSecretEnv = "CONCLAVE_GITHUB_WEBHOOK_SECRET"
	// AppKeyFileEnv names the variable holding the path of the App's
	// private key file. Only the worker reads it.
	AppKeyFileEnv = "CONCLAVE_GITHUB_APP_KEY_FILE"
	// MinWebhookSecretLen is the shortest webhook secret accepted.
	MinWebhookSecretLen = 32
)

// ErrInvalidConfig wraps every configuration validation failure.
var ErrInvalidConfig = errors.New("invalid GitHub configuration")

// Config is the owner's GitHub App configuration, from deploy/github.yaml.
// None of it is secret: the webhook secret and the private key come from
// the environment (WebhookSecretEnv, AppKeyFileEnv).
type Config struct {
	// AppID is the App's numeric ID; it is the JWT issuer, and a comment
	// whose performed_via_github_app carries it is the platform's own.
	AppID int64 `yaml:"app_id"`
	// AppSlug is the App's URL name. Its bot user is "<slug>[bot]".
	AppSlug string `yaml:"app_slug"`
	// InstallationID is the App's one installation. Deliveries from any
	// other installation are ignored.
	InstallationID int64 `yaml:"installation_id"`
	// OwnerUserID is the owner's numeric GitHub user ID, the only account
	// whose labels and commands are honoured. Never a login: logins change
	// (FR-SEC1).
	OwnerUserID int64 `yaml:"owner_user_id"`
	// TriggerLabel starts a run when the owner applies it.
	TriggerLabel string `yaml:"trigger_label"`
	// APIBaseURL and GitBaseURL may be empty for GitHub's own endpoints.
	// Both must be https, except on a loopback host, where tests run.
	APIBaseURL string `yaml:"api_base_url"`
	GitBaseURL string `yaml:"git_base_url"`
}

var appSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

// LoadConfig parses and validates YAML configuration. Unknown fields are an
// error, so a misspelt key fails at startup instead of being ignored, and
// empty base URLs and trigger label take their defaults.
func LoadConfig(r io.Reader) (Config, error) {
	var cfg Config
	if err := decodeStrict(r, &cfg); err != nil {
		return Config{}, fmt.Errorf("github configuration: %w", err)
	}
	if cfg.TriggerLabel == "" {
		cfg.TriggerLabel = DefaultTriggerLabel
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = DefaultAPIBaseURL
	}
	if cfg.GitBaseURL == "" {
		cfg.GitBaseURL = DefaultGitBaseURL
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// decodeStrict decodes YAML into v with the same settings as every other
// configuration file (decision 0007): unknown keys are an error.
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

// Validate reports the first missing or malformed setting. The placeholder
// file leaves the IDs at zero, so the server refuses to start until the
// owner fills them in.
func (c Config) Validate() error {
	switch {
	case c.AppID <= 0:
		return fmt.Errorf("%w: app_id must be set", ErrInvalidConfig)
	case !appSlug.MatchString(c.AppSlug):
		return fmt.Errorf("%w: app_slug %q is not an App slug", ErrInvalidConfig, c.AppSlug)
	case c.InstallationID <= 0:
		return fmt.Errorf("%w: installation_id must be set", ErrInvalidConfig)
	case c.OwnerUserID <= 0:
		return fmt.Errorf("%w: owner_user_id must be set", ErrInvalidConfig)
	case strings.TrimSpace(c.TriggerLabel) == "":
		return fmt.Errorf("%w: trigger_label must be set", ErrInvalidConfig)
	}
	for _, base := range []string{c.APIBaseURL, c.GitBaseURL} {
		if err := checkBaseURL(base); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
	}
	return nil
}

// checkBaseURL requires https, or plain http to a loopback host, where
// tests run their servers. A token travels in a header to this host, and
// plain http anywhere else would send it in the clear.
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parsing base URL: %w", err)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("base URL %q must be scheme and host only", u.Redacted())
	}
	if u.Scheme == "https" && u.Host != "" {
		return nil
	}
	if u.Scheme == "http" && isLoopback(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("base URL %q must be https", u.Redacted())
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// fullName matches an owner/name pair as GitHub allows them. It is checked
// before a full name is placed in a URL path or a git remote, so a payload
// or caller cannot steer a request elsewhere with "..", "?" or "@".
var fullName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)

func checkFullName(name string) error {
	repo := name[strings.IndexByte(name, '/')+1:]
	if !fullName.MatchString(name) || repo == "." || repo == ".." || strings.HasSuffix(repo, ".git") {
		return fmt.Errorf("%w: repository name %q", ErrRefused, name)
	}
	return nil
}
