package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
)

func TestCheckProtected(t *testing.T) {
	mod := func(p string) FileChange {
		return FileChange{Kind: ChangeModified, OldPath: p, NewPath: p, OldMode: "100644", NewMode: "100644", Added: []string{"x"}}
	}
	cases := []struct {
		name   string
		change FileChange
		pass   bool
		detail string
	}{
		{"ordinary file", mod("src/App.java"), true, "no protected paths"},
		{"workflow", mod(".github/workflows/ci.yml"), false, `".github/workflows/ci.yml" (matches ".github/")`},
		{"nested CODEOWNERS", mod("docs/CODEOWNERS"), false, "CODEOWNERS"},
		{"case-folded", mod(".GitHub/workflows/ci.yml"), false, ".github/"},
		{"gitleaks config", mod(".gitleaks.toml"), false, ".gitleaks*"},
		{"gitleaksignore added", FileChange{Kind: ChangeAdded, NewPath: ".gitleaksignore", NewMode: "100644"}, false, ".gitleaks*"},
		{"jenkinsfile deleted", FileChange{Kind: ChangeDeleted, OldPath: "Jenkinsfile", OldMode: "100644"}, false, "Jenkinsfile"},
		{"renamed into protected", FileChange{Kind: ChangeRenamed, OldPath: "ci.yml", NewPath: ".github/workflows/ci.yml"}, false, ".github/"},
		{"renamed out of protected", FileChange{Kind: ChangeRenamed, OldPath: ".github/workflows/ci.yml", NewPath: "ci.yml"}, false, ".github/"},
		{"copied from protected", FileChange{Kind: ChangeCopied, OldPath: ".travis.yml", NewPath: "travis.yml"}, false, ".travis.yml"},
		{"repo addition", mod("infra/main.tf"), false, `"infra/"`},
		{"new symlink", FileChange{Kind: ChangeAdded, NewPath: "link", NewMode: modeSymlink, Added: []string{"../x"}}, false, "symlink or gitlink"},
		{"retargeted symlink", FileChange{Kind: ChangeModified, OldPath: "l", NewPath: "l", OldMode: modeSymlink, NewMode: modeSymlink}, false, "symlink or gitlink"},
		{"file became symlink", FileChange{Kind: ChangeModified, OldPath: "l", NewPath: "l", OldMode: "100644", NewMode: modeSymlink}, false, "symlink or gitlink"},
		{"new gitlink", FileChange{Kind: ChangeAdded, NewPath: "sub", NewMode: modeGitlink}, false, "symlink or gitlink"},
		{"deleted symlink", FileChange{Kind: ChangeDeleted, OldPath: "l", OldMode: modeSymlink}, true, "no protected paths"},
		{"invalid path", mod("a/../b"), false, "invalid path"},
		{"git dir", mod(".git/config"), false, "invalid path"},
	}
	p := testPolicy(t)
	p.ProtectedPaths = []string{"infra/"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := checkProtected([]FileChange{tc.change}, p)
			if v.Gate != contracts.GateProtected || v.Passed != tc.pass || v.Flagged {
				t.Fatalf("got %+v, want passed=%v and never flagged", v, tc.pass)
			}
			if !strings.Contains(v.Detail, tc.detail) {
				t.Fatalf("detail %q does not contain %q", v.Detail, tc.detail)
			}
		})
	}
}

func TestCheckProtected_DetailIsBounded(t *testing.T) {
	var changes []FileChange
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		changes = append(changes, FileChange{Kind: ChangeAdded, NewPath: ".github/" + name, NewMode: "100644"})
	}
	v := checkProtected(changes, testPolicy(t))
	if v.Passed || !strings.HasSuffix(v.Detail, "and 2 more") {
		t.Fatalf("got %+v, want five paths and a count", v)
	}
}

func TestCheckProtected_Fixture(t *testing.T) {
	diff, err := os.ReadFile(filepath.Join("testdata", "fail_protected_paths.diff"))
	if err != nil {
		t.Fatal(err)
	}
	if v := checkProtected(mustParse(t, diff), testPolicy(t)); v.Passed {
		t.Fatalf("fail_protected_paths.diff passed: %+v", v)
	}
}
