package llm_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/faketest"
)

// sentinel is one exported error of package llm and whether a retry can fix
// it.
type sentinel struct {
	name      string
	err       error
	permanent bool
}

// sentinels classifies every exported error in package llm.
// TestEverySentinelIsClassified fails when the package gains an Err*
// variable missing from this table, so a new error cannot be left out of
// IsPermanent by omission.
var sentinels = []sentinel{
	// Policy and configuration errors. The owner's configuration or the
	// request is wrong, and a retry sends the same thing again.
	{"ErrUnknownRepo", llm.ErrUnknownRepo, true},
	{"ErrNoTier", llm.ErrNoTier, true},
	{"ErrUnknownTier", llm.ErrUnknownTier, true},
	{"ErrNoRoleAllowlist", llm.ErrNoRoleAllowlist, true},
	{"ErrRoleDenied", llm.ErrRoleDenied, true},
	{"ErrUnknownRole", llm.ErrUnknownRole, true},
	{"ErrTierDenied", llm.ErrTierDenied, true},
	{"ErrUnknownProvider", llm.ErrUnknownProvider, true},
	{"ErrTierMismatch", llm.ErrTierMismatch, true},
	{"ErrNoEmbeddingModel", llm.ErrNoEmbeddingModel, true},
	{"ErrInvalidConfig", llm.ErrInvalidConfig, true},

	// Router errors.
	{"ErrNoRoute", llm.ErrNoRoute, true},
	{"ErrBudgetExceeded", llm.ErrBudgetExceeded, true},
	{"ErrInvalidRequest", llm.ErrInvalidRequest, true},
	{"ErrMissingProvider", llm.ErrMissingProvider, true},
	{"ErrInvalidScope", llm.ErrInvalidScope, true},
	{"ErrRepoMismatch", llm.ErrRepoMismatch, true},

	// Provider errors.
	{"ErrAuth", llm.ErrAuth, true},
	{"ErrUnsupportedReply", llm.ErrUnsupportedReply, true},
	{"ErrUnavailable", llm.ErrUnavailable, false},
	{"ErrRateLimited", llm.ErrRateLimited, false},
	{"ErrMalformedReply", llm.ErrMalformedReply, false},
}

// TestIsPermanent checks every sentinel bare and wrapped, since callers see
// both, plus the concrete errors adapters and fakes return.
func TestIsPermanent(t *testing.T) {
	forms := []struct {
		name string
		wrap func(error) error
	}{
		{"bare", func(err error) error { return err }},
		{"wrapped", func(err error) error { return fmt.Errorf("call: %w", err) }},
		{"wrapped twice", func(err error) error { return fmt.Errorf("activity: %w", fmt.Errorf("call: %w", err)) }},
	}
	for _, s := range sentinels {
		for _, form := range forms {
			t.Run(s.name+"/"+form.name, func(t *testing.T) {
				err := form.wrap(s.err)
				if got := llm.IsPermanent(err); got != s.permanent {
					t.Fatalf("IsPermanent(%v) = %v, want %v", err, got, s.permanent)
				}
			})
		}
	}

	others := []struct {
		name string
		err  error
		want bool
	}{
		{"rate limit with retry-after", &llm.RateLimitError{RetryAfter: time.Second}, false},
		{"fake rejected reply", faketest.ErrRejected, true},
		{"unclassified error", errors.New("database down"), false},
		{"nil", nil, false},
	}
	for _, tc := range others {
		t.Run(tc.name, func(t *testing.T) {
			if got := llm.IsPermanent(tc.err); got != tc.want {
				t.Fatalf("IsPermanent(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestEverySentinelIsClassified compares the sentinels table with the Err*
// variables the package actually declares.
func TestEverySentinelIsClassified(t *testing.T) {
	declared := declaredSentinels(t)
	var classified []string
	for _, s := range sentinels {
		classified = append(classified, s.name)
	}
	slices.Sort(classified)
	if !slices.Equal(declared, classified) {
		t.Fatalf("package declares %v, table classifies %v", declared, classified)
	}
}

// declaredSentinels returns the sorted names of the exported Err* package
// variables in the non-test files of the current directory.
func declaredSentinels(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		names = append(names, errVars(f)...)
	}
	slices.Sort(names)
	return names
}

func errVars(f *ast.File) []string {
	var names []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			names = append(names, errNames(spec)...)
		}
	}
	return names
}

func errNames(spec ast.Spec) []string {
	vs, ok := spec.(*ast.ValueSpec)
	if !ok {
		return nil
	}
	var names []string
	for _, id := range vs.Names {
		if strings.HasPrefix(id.Name, "Err") && id.IsExported() {
			names = append(names, id.Name)
		}
	}
	return names
}

// TestPolicyErrorsArePermanent drives each bad repository policy through
// the paths that validate it, at startup and per request, and checks the
// error that comes out is permanent. A per-request policy error that is not
// would be retried by Temporal instead of failing the attempt.
func TestPolicyErrorsArePermanent(t *testing.T) {
	cases := append(slices.Clone(badPolicies), badPolicy{"unconfigured repository", llm.StaticRepos{}, llm.ErrUnknownRepo})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, plannerConfig(), fakes(), liveRepos(tc.repos), 100000)
			_, err := complete(h, contracts.RolePlanner)
			assertPermanent(t, "Complete", err, tc.want)
			_, err = h.scoped.Embed(context.Background(), repoID, []string{"x"})
			assertPermanent(t, "Embed", err, tc.want)
			if len(tc.repos) == 0 {
				return
			}
			_, err = llm.New(plannerConfig(), providers(fakes()), tc.repos, newCosts())
			assertPermanent(t, "New", err, tc.want)
		})
	}
}

// TestConfigErrorsArePermanent: a configuration file the loaders reject is
// permanent, including a policy error found while loading one.
func TestConfigErrorsArePermanent(t *testing.T) {
	cases := []struct {
		name string
		load func() error
		want error
	}{
		{"router config without roles", func() error {
			_, err := llm.LoadConfig(strings.NewReader("models: {}\n"))
			return err
		}, llm.ErrInvalidConfig},
		{"access file with a misspelt role", func() error {
			_, err := llm.LoadRepoAccess(strings.NewReader("repositories:\n  a/b:\n    tier: public\n    roles: [implementor]\n"))
			return err
		}, llm.ErrUnknownRole},
		{"access file that is not YAML", func() error {
			_, err := llm.LoadRepoAccess(strings.NewReader("repositories: [\n"))
			return err
		}, llm.ErrInvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertPermanent(t, "load", tc.load(), tc.want)
		})
	}
}

func assertPermanent(t *testing.T, step string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) || !llm.IsPermanent(err) {
		t.Fatalf("%s: got %v, want permanent %v", step, err, want)
	}
}
