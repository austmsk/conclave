package docker

import (
	"errors"
	"testing"
)

func TestWorkspacePathRejectsEscapes(t *testing.T) {
	rejected := []struct{ name, path string }{
		{"empty", ""},
		{"absolute", "/etc/passwd"},
		{"parent", "../secret"},
		{"nested parent", "src/../../secret"},
		{"trailing parent", "src/.."},
		{"dot", "."},
		{"dot component", "src/./x"},
		{"doubled traversal", "....//etc/passwd"},
		{"dots only component", "src/..../x"},
		{"double slash", "src//x"},
		{"trailing slash", "src/"},
		{"backslash", "src\\..\\x"},
		{"nul", "src\x00x"},
		{"newline", "src\nx"},
		{"encoded dot", "%2e%2e/secret"},
		{"encoded slash", "..%2fsecret"},
		{"mixed case encoded", "%2E%2E/secret"},
		{"git dir", ".git"},
		{"git internals", ".git/config"},
		{"git hooks", ".git/hooks/pre-commit"},
		{"nested git", "vendor/.git/config"},
		{"too long", string(make([]byte, maxPathLength+1))},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := workspacePath(tc.path); !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("workspacePath(%q) = %v, want ErrInvalidPath", tc.path, err)
			}
		})
	}
}

func TestWorkspacePathAcceptsOrdinaryPaths(t *testing.T) {
	accepted := []string{
		"README.md",
		"src/tally.go",
		"a/b/c/d.txt",
		".gitignore",
		".github/workflows/ci.yml",
		"src/.hidden",
		"dir.with.dots/file..name",
		"file%.txt",
		"with space/name.txt",
		"unicode/日本語.txt",
		// Glob characters are literal: the script never expands them, so
		// the validator has no reason to refuse a legal filename.
		"a*b.txt",
		"q?.txt",
		"[set].txt",
		".gi*/config",
	}
	for _, p := range accepted {
		got, err := workspacePath(p)
		if err != nil {
			t.Errorf("workspacePath(%q) = %v, want accepted", p, err)
		}
		if got != p {
			t.Errorf("workspacePath(%q) rewrote the path to %q", p, got)
		}
	}
}
