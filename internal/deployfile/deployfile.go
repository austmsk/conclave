package deployfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// PlaceholderRepo is the repository key every shipped deploy file carries.
// It fails CheckRepoName, so the worker refuses to start until the owner
// fills in a real name.
const PlaceholderRepo = "owner/repo"

// fullName is a GitHub owner/repo: an owner of letters, digits and
// hyphens, and a repository name of letters, digits, '.', '_' and '-'.
var fullName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// Decode decodes exactly one YAML document from r into v. Unknown keys
// are an error, so a misspelt setting stops startup instead of being
// ignored, and so is a second document, which would otherwise be dropped
// unread. Parse failures wrap invalid, the caller's sentinel; a read
// failure does not, since the file was never seen.
func Decode(r io.Reader, v any, invalid error) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: parsing YAML: %w", invalid, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: more than one YAML document", invalid)
	}
	return nil
}

// CheckRepoName refuses the placeholder and anything that is not a
// GitHub full name.
func CheckRepoName(name string) error {
	switch {
	case name == PlaceholderRepo:
		return errors.New("placeholder: replace owner/repo with the repository's full name")
	case !fullName.MatchString(name):
		return errors.New("not a repository full name of the form owner/repo")
	case strings.HasSuffix(name, "/.") || strings.HasSuffix(name, "/.."):
		return errors.New("not a valid repository name")
	}
	return nil
}

// RepoKeys returns m's keys in sorted order after checking each with
// CheckRepoName. GitHub names are case-insensitive, so two keys that
// differ only in case would be one repository with two configurations,
// and are refused. Errors start with the key, as repositories."<name>",
// for the caller to wrap with its sentinel.
func RepoKeys[V any](m map[string]V) ([]string, error) {
	keys := slices.Sorted(maps.Keys(m))
	seen := make(map[string]string, len(keys))
	for _, name := range keys {
		if err := CheckRepoName(name); err != nil {
			return nil, fmt.Errorf("repositories.%q: %w", name, err)
		}
		if other, dup := seen[strings.ToLower(name)]; dup {
			return nil, fmt.Errorf("repositories.%q: same repository as %q", name, other)
		}
		seen[strings.ToLower(name)] = name
	}
	return keys, nil
}

// Load reads the file at path, hands its bytes to load, and returns the
// lowercase hex SHA-256 of exactly those bytes. Reading once and hashing
// what was loaded means the digest names the configuration that passed
// validation, not a later version of the file. Errors name the path.
func Load(path string, load func(io.Reader) error) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	if err := load(bytes.NewReader(data)); err != nil {
		return "", fmt.Errorf("loading %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
