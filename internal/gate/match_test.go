package gate

import (
	"errors"
	"testing"
)

func TestPatternMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		// A name with no inner slash matches at any depth.
		{"Makefile", "Makefile", true},
		{"Makefile", "sub/dir/Makefile", true},
		{"Makefile", "Makefile/inner.txt", true},
		{"Makefile", "Makefile.old", false},
		{"*.mk", "build/rules.mk", true},
		{"*.mk", "rules.mkx", false},
		{"Dockerfile*", "deploy/Dockerfile.dev", true},
		{"lefthook.y*ml", "lefthook.yml", true},
		{"lefthook.y*ml", "lefthook.yaml", true},
		{".gitleaks*", ".gitleaksignore", true},
		{".gitleaks*", "cfg/.gitleaks.toml", true},
		// A trailing slash means a directory.
		{".husky/", ".husky/pre-commit", true},
		{".husky/", "web/.husky/_/husky.sh", true},
		{".husky/", ".husky", false},
		{".husky/", "husky/pre-commit", false},
		// An inner slash anchors at the root.
		{"gradle/wrapper/", "gradle/wrapper/gradle-wrapper.jar", true},
		{"gradle/wrapper/", "sub/gradle/wrapper/gradle-wrapper.jar", false},
		{"src/*Test.java", "src/FooTest.java", true},
		{"src/*Test.java", "src/sub/FooTest.java", false},
		{"src/*Test.java", "other/src/FooTest.java", false},
		{"src/*Test.java", "src/Foo.java", false},
		// Folding: case, HFS+-ignorable code points, NTFS trailing dots.
		{".claude/", ".Claude/settings.json", true},
		{".claude/", ".CLAUDE/settings.json", true},
		{".claude/", ".cl\u200caude/settings.json", true},
		{".claude/", ".claude\ufeff/settings.json", true},
		{"Makefile", "makefile", true},
		{"Makefile", "MAKEFILE.", true},
		{"CLAUDE.md", "docs/claude.MD", true},
		{"src/*Test.java", "SRC/FooTEST.java", true},
		// path.Match syntax within a component.
		{"?.txt", "a.txt", true},
		{"?.txt", "ab.txt", false},
		{"[ab].txt", "b.txt", true},
		{"*", "anything/at/all", true},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+" "+tc.path, func(t *testing.T) {
			p, err := compilePattern(tc.pattern)
			if err != nil {
				t.Fatalf("compilePattern: %v", err)
			}
			if got := p.match(tc.path); got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompilePattern_Refused(t *testing.T) {
	for _, raw := range []string{"", "/", "/abs", "**", "src/**/x", "a**b", "a//b", "./a", "a/../b", "a\\b", "[", "a/[/b", "..."} {
		t.Run(raw, func(t *testing.T) {
			if _, err := compilePattern(raw); !errors.Is(err, errBadPattern) {
				t.Fatalf("got %v, want errBadPattern", err)
			}
		})
	}
}

// TestDefaultPatternsCompile guards the floors: a typo there would stop
// every policy from loading.
func TestDefaultPatternsCompile(t *testing.T) {
	if _, err := compilePatterns(defaultProtectedPaths, defaultExecutionSurfacePaths); err != nil {
		t.Fatal(err)
	}
}
