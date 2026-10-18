package sandbox

import (
	"errors"
	"testing"
)

func TestCheckPathRejects(t *testing.T) {
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
		{"delete", "src\x7fx"},
		{"c1 control", "src\u0085x"},
		{"c1 csi", "src\u009bx"},
		{"invalid utf8", "src/\xff.txt"},
		{"truncated utf8", "src/\xe6\x97.txt"},
		{"bidi override", "src/\u202egnp.exe"},
		{"bidi embedding", "src/\u202ax"},
		{"bidi isolate", "src/\u2066x\u2069"},
		{"left to right mark", "src/\u200ex"},
		{"right to left mark", "src/\u200fx"},
		{"arabic letter mark", "src/\u061cx"},
		{"encoded dot", "%2e%2e/secret"},
		{"encoded slash", "..%2fsecret"},
		{"mixed case encoded", "%2E%2E/secret"},
		{"git dir", ".git"},
		{"git internals", ".git/config"},
		{"git hooks", ".git/hooks/pre-commit"},
		{"nested git", "vendor/.git/config"},
		{"upper case git", ".GIT/config"},
		{"mixed case git", ".Git/hooks/pre-commit"},
		{"trailing dot git", ".git./config"},
		{"trailing space git", ".git /config"},
		{"trailing dots and spaces git", ".Git. ./config"},
		{"ntfs stream git", ".git::$INDEX_ALLOCATION/config"},
		{"hfs zero width non joiner", ".g\u200cit/config"},
		{"hfs zero width joiner", ".gi\u200dt/config"},
		{"hfs inhibit swapping", "\u206a.git/config"},
		{"hfs byte order mark", ".git\ufeff/config"},
		{"hfs ignorable and case", ".G\u200cIT./config"},
		{"ntfs short name", "git~1/config"},
		{"ntfs short name upper", "GIT~1/config"},
		{"ntfs short name trailing dot", "Git~1./config"},
		{"nested ntfs short name", "vendor/git~1/hooks"},
		{"too long", string(make([]byte, maxPathLength+1))},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckPath(tc.path); !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("CheckPath(%q) = %v, want ErrInvalidPath", tc.path, err)
			}
		})
	}
}

func TestCheckPathAcceptsOrdinaryPaths(t *testing.T) {
	accepted := []string{
		"README.md",
		"src/tally.go",
		"a/b/c/d.txt",
		".gitignore",
		".gitattributes",
		".gitmodules",
		".github/workflows/ci.yml",
		".GitHub/x",
		"git/config",
		"x.git/config",
		"git~2/config",
		"src/.hidden",
		"dir.with.dots/file..name",
		"file%.txt",
		"with space/name.txt",
		"unicode/日本語.txt",
		"emoji/\U0001F469\u200d\U0001F4BB.txt",
		"-f",
		"a:b.txt",
		// Glob characters are literal: the script never expands them, so
		// the validator has no reason to refuse a legal filename.
		"a*b.txt",
		"q?.txt",
		"[set].txt",
		".gi*/config",
	}
	for _, p := range accepted {
		if err := CheckPath(p); err != nil {
			t.Errorf("CheckPath(%q) = %v, want accepted", p, err)
		}
	}
}

func TestFoldName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Secrets", "secrets"},
		{"secrets.", "secrets"},
		{"secrets . ", "secrets"},
		{"sec\u200crets", "secrets"},
		{"secrets::$DATA", "secrets"},
		{"prod.yaml", "prod.yaml"},
	}
	for _, tc := range cases {
		if got := FoldName(tc.in); got != tc.want {
			t.Errorf("FoldName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
