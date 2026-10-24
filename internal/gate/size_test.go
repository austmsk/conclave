package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
)

func w1Lines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "x"
	}
	return out
}

func TestCheckDiffSize(t *testing.T) {
	lim := SizeLimits{MaxChangedLines: 10, MaxChangedFiles: 2, MaxDiffBytes: 1000}
	text := func(added, removed int) FileChange {
		return FileChange{Kind: ChangeModified, OldPath: "f", NewPath: "f", Added: w1Lines(added), Removed: w1Lines(removed)}
	}
	bin := FileChange{Kind: ChangeAdded, NewPath: "b.bin", Binary: true}
	cases := []struct {
		name    string
		changes []FileChange
		bytes   int
		lim     SizeLimits
		pass    bool
		detail  string
	}{
		{"within limits", []FileChange{text(3, 2)}, 100, lim, true, "1 files, 5 lines, 100 bytes"},
		{"at every limit", []FileChange{text(5, 0), text(0, 5)}, 1000, lim, true, "2 files"},
		{"empty diff", nil, 0, lim, false, "empty diff"},
		{"too many lines", []FileChange{text(6, 5)}, 100, lim, false, "11 changed lines exceed 10"},
		{"too many files", []FileChange{text(1, 0), text(1, 0), bin}, 100, lim, false, "3 changed files exceed 2"},
		{"binary counts as a file", []FileChange{bin, bin, bin}, 100, lim, false, "3 changed files exceed 2"},
		{"too many bytes", []FileChange{text(1, 0)}, 1001, lim, false, "1001 diff bytes exceed 1000"},
		{"limits unset", []FileChange{text(1, 0)}, 10, SizeLimits{}, false, "not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := checkDiffSize(tc.changes, tc.bytes, tc.lim)
			if v.Gate != contracts.GateDiffSize || v.Passed != tc.pass || v.Flagged {
				t.Fatalf("got %+v, want passed=%v", v, tc.pass)
			}
			if !strings.Contains(v.Detail, tc.detail) {
				t.Fatalf("detail %q does not contain %q", v.Detail, tc.detail)
			}
		})
	}
}

func TestCheckDiffSize_Fixture(t *testing.T) {
	diff, err := os.ReadFile(filepath.Join("testdata", "fail_diff_size.diff"))
	if err != nil {
		t.Fatal(err)
	}
	p := testPolicy(t)
	if v := checkDiffSize(mustParse(t, diff), len(diff), p.Limits); v.Passed {
		t.Fatalf("fail_diff_size.diff passed: %+v", v)
	}
}
