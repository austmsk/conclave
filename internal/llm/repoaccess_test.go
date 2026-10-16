package llm_test

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
)

const validAccessYAML = `
repositories:
  austmsk/election-tally:
    tier: sensitive
    roles: [planner]
    sensitive_providers: [anthropic]
  austmsk/site:
    tier: public
    roles: [planner, implementer]
`

func TestLoadRepoAccessValid(t *testing.T) {
	access, err := llm.LoadRepoAccess(strings.NewReader(validAccessYAML))
	if err != nil {
		t.Fatalf("LoadRepoAccess: %v", err)
	}
	tally := access["austmsk/election-tally"]
	if tally.Tier != contracts.TierSensitive || !slices.Equal(tally.Roles, []contracts.Role{contracts.RolePlanner}) ||
		!slices.Equal(tally.SensitiveProviders, []string{llm.ProviderAnthropic}) {
		t.Fatalf("election-tally access %+v", tally)
	}
	if site := access["austmsk/site"]; site.Tier != contracts.TierPublic || len(site.Roles) != 2 {
		t.Fatalf("site access %+v", site)
	}
}

// TestLoadRepoAccessRejects: every mistake the owner could make in the file
// stops startup. want is the specific error, when there is one; every case
// also wraps ErrInvalidConfig.
func TestLoadRepoAccessRejects(t *testing.T) {
	cases := []struct {
		name    string
		replace [2]string
		want    error
	}{
		{"misspelt field", [2]string{"sensitive_providers:", "sensitive_provider:"}, nil},
		{"duplicate repository", [2]string{"austmsk/site:", "austmsk/election-tally:"}, nil},
		{"no tier", [2]string{"    tier: public\n", ""}, llm.ErrNoTier},
		{"unknown tier", [2]string{"tier: public", "tier: internal"}, llm.ErrUnknownTier},
		{"no role allowlist", [2]string{"    roles: [planner, implementer]\n", ""}, llm.ErrNoRoleAllowlist},
		{"misspelt role", [2]string{"[planner, implementer]", "[planner, implementor]"}, llm.ErrUnknownRole},
		{"misspelt provider", [2]string{"[anthropic]", "[anthropc]"}, llm.ErrUnknownProvider},
		{"empty repository name", [2]string{"austmsk/site:", `"":`}, llm.ErrUnknownRepo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(validAccessYAML, tc.replace[0], tc.replace[1], 1)
			if src == validAccessYAML {
				t.Fatalf("replacement %q did not apply", tc.replace[0])
			}
			assertInvalidAccess(t, src, tc.want)
		})
	}
}

func TestLoadRepoAccessNeedsRepositories(t *testing.T) {
	for _, src := range []string{"", "repositories: {}\n"} {
		assertInvalidAccess(t, src, nil)
	}
}

func assertInvalidAccess(t *testing.T, src string, want error) {
	t.Helper()
	_, err := llm.LoadRepoAccess(strings.NewReader(src))
	if !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("got %v, want ErrInvalidConfig", err)
	}
	if want != nil && !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// TestDeployedRepoAccess keeps deploy/repos.yaml loadable. It must fail as
// shipped, because its placeholder is meant to stop startup, and must load
// once the placeholder is filled in.
func TestDeployedRepoAccess(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/repos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := llm.LoadRepoAccess(bytes.NewReader(raw)); !errors.Is(err, llm.ErrNoTier) {
		t.Fatalf("shipped file with placeholder: got %v, want ErrNoTier", err)
	}
	filled := strings.Replace(string(raw), "    tier: \"\"\n", "    tier: public\n", 1)
	if filled == string(raw) {
		t.Fatal("placeholder tier not found")
	}
	access, err := llm.LoadRepoAccess(strings.NewReader(filled))
	if err != nil {
		t.Fatalf("file with placeholder filled: %v", err)
	}
	if access["owner/repo"].Tier != contracts.TierPublic {
		t.Fatalf("access %+v", access)
	}
}
