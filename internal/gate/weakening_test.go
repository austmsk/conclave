package gate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
)

// TestGateWeakening runs real-shaped diffs through ParseDiff and the gate
// under the ETS-like testPolicy: tests are src/*Test.java and lib/ is
// test configuration.
func TestGateWeakening(t *testing.T) {
	const idx = "index 1111111..2222222 100644\n"
	cases := []struct {
		name, diff string
		pass       bool
		detail     string
	}{
		{"deleted_test_file", "diff --git a/src/FooTest.java b/src/FooTest.java\ndeleted file mode 100644\nindex 1111111..0000000\n" +
			"--- a/src/FooTest.java\n+++ /dev/null\n@@ -1,3 +0,0 @@\n-class FooTest {\n-    @Test void adds() {}\n-}\n", false, "test deleted"},
		{"renamed_out_of_tests", "diff --git a/src/FooTest.java b/src/Foo.txt\nsimilarity index 100%\nrename from src/FooTest.java\nrename to src/Foo.txt\n",
			false, "renamed out of the test paths"},
		{"case_marker_removed", "diff --git a/src/FooTest.java b/src/FooTest.java\n" + idx +
			"--- a/src/FooTest.java\n+++ b/src/FooTest.java\n@@ -1,5 +1,3 @@\n class FooTest {\n     @Test void adds() {}\n-    @Test\n-    void subtracts() {}\n }\n",
			false, "1 test cases removed, 0 added"},
		{"commented_out_test", "diff --git a/src/FooTest.java b/src/FooTest.java\n" + idx +
			"--- a/src/FooTest.java\n+++ b/src/FooTest.java\n@@ -1,4 +1,4 @@\n class FooTest {\n-    @Test\n-    void adds() { assertEquals(2, add(1, 1)); }\n+    // @Test\n+    // void adds() { assertEquals(2, add(1, 1)); }\n }\n",
			false, "1 test cases removed, 0 added"},
		{"skip_marker_added", "diff --git a/src/FooTest.java b/src/FooTest.java\n" + idx +
			"--- a/src/FooTest.java\n+++ b/src/FooTest.java\n@@ -1,3 +1,4 @@\n class FooTest {\n+    @Disabled(\"flaky\")\n     @Test void adds() {}\n }\n",
			false, "skip marker added"},
		{"test_config_changed", "diff --git a/lib/junit-platform.properties b/lib/junit-platform.properties\n" + idx +
			"--- a/lib/junit-platform.properties\n+++ b/lib/junit-platform.properties\n@@ -1 +1 @@\n-a=1\n+junit.jupiter.conditions.deactivate=*\n",
			false, "test configuration changed"},
		{"assumption added", "diff --git a/src/FooTest.java b/src/FooTest.java\n" + idx +
			"--- a/src/FooTest.java\n+++ b/src/FooTest.java\n@@ -1,2 +1,3 @@\n class FooTest {\n+    { assumeTrue(false); }\n }\n",
			false, "skip marker added"},
		{"test made binary", "diff --git a/src/FooTest.java b/src/FooTest.java\nindex 1111111..2222222 100644\nBinary files a/src/FooTest.java and b/src/FooTest.java differ\n",
			false, "test changed as binary"},
		{"test added", "diff --git a/src/BarTest.java b/src/BarTest.java\nnew file mode 100644\nindex 0000000..2222222\n" +
			"--- /dev/null\n+++ b/src/BarTest.java\n@@ -0,0 +1,3 @@\n+class BarTest {\n+    @Test void bars() {}\n+}\n", true, "no tests deleted"},
		{"test moved between files", "diff --git a/src/ATest.java b/src/ATest.java\n" + idx +
			"--- a/src/ATest.java\n+++ b/src/ATest.java\n@@ -1,3 +1,2 @@\n class ATest {\n-    @Test void moved() {}\n }\n" +
			"diff --git a/src/BTest.java b/src/BTest.java\n" + idx +
			"--- a/src/BTest.java\n+++ b/src/BTest.java\n@@ -1,2 +1,3 @@\n class BTest {\n+    @Test void moved() {}\n }\n", true, "no tests deleted"},
		{"test renamed within tests", "diff --git a/src/OldTest.java b/src/NewTest.java\nsimilarity index 100%\nrename from src/OldTest.java\nrename to src/NewTest.java\n",
			true, "no tests deleted"},
		{"skip marker outside tests", "diff --git a/src/App.java b/src/App.java\n" + idx +
			"--- a/src/App.java\n+++ b/src/App.java\n@@ -1 +1,2 @@\n class App {}\n+// @Disabled is a JUnit annotation\n", true, "no tests deleted"},
		{"case-folded test path deleted", "diff --git a/SRC/FooTEST.java b/SRC/FooTEST.java\ndeleted file mode 100644\nindex 1111111..0000000\n" +
			"--- a/SRC/FooTEST.java\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n", false, "test deleted"},
	}
	p := testPolicy(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := checkWeakening(mustParse(t, []byte(tc.diff)), p)
			if v.Gate != contracts.GateWeakening || v.Passed != tc.pass || v.Flagged {
				t.Fatalf("got %+v, want passed=%v", v, tc.pass)
			}
			if !strings.Contains(v.Detail, tc.detail) {
				t.Fatalf("detail %q does not contain %q", v.Detail, tc.detail)
			}
		})
	}
}

func TestCheckWeakening_Unconfigured(t *testing.T) {
	change := []FileChange{{Kind: ChangeModified, OldPath: "a", NewPath: "a", Added: []string{"x"}}}
	if v := checkWeakening(change, Policy{}); v.Passed {
		t.Fatalf("a policy without test paths passed: %+v", v)
	}
}

func TestCountCaseMarkers(t *testing.T) {
	markers := []*regexp.Regexp{regexp.MustCompile(`@Test\b`), regexp.MustCompile(`^\s*func Test`)}
	lines := []string{
		"    @Test", "\t@Test void x() {}", "@Test", // counted
		"func TestX(t *testing.T) {",                                                               // counted: anchored marker on the trimmed line
		"// @Test", "# @Test", "x = \"@Test\"", "    /* @Test */", "@Testing", "@Timeout(5) @Test", // not counted
	}
	if got := countCaseMarkers(lines, markers); got != 4 {
		t.Fatalf("counted %d, want 4", got)
	}
}

// TestCheckWeakening_DetailOmitsContent checks the Detail names paths
// only: line content is the agent's.
func TestCheckWeakening_DetailOmitsContent(t *testing.T) {
	change := []FileChange{{Kind: ChangeModified, OldPath: "src/FooTest.java", NewPath: "src/FooTest.java",
		Added: []string{"@Disabled canary_gate_detail"}}}
	v := checkWeakening(change, testPolicy(t))
	if v.Passed || strings.Contains(v.Detail, "canary_") {
		t.Fatalf("got %+v", v)
	}
}

func TestCheckWeakening_Fixture(t *testing.T) {
	diff, err := os.ReadFile(filepath.Join("testdata", "fail_gate_weakening.diff"))
	if err != nil {
		t.Fatal(err)
	}
	if v := checkWeakening(mustParse(t, diff), testPolicy(t)); v.Passed {
		t.Fatalf("fail_gate_weakening.diff passed: %+v", v)
	}
}
