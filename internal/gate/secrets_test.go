package gate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/contracts"
)

func TestSecretsInput(t *testing.T) {
	cases := []struct {
		name       string
		changes    []FileChange
		wantInput  string
		wantBinary []string
	}{
		{
			name: "added lines under a header per file, removed lines left out",
			changes: []FileChange{
				{Kind: ChangeModified, OldPath: "src/a.go", NewPath: "src/a.go", Added: []string{"x := 1", "y := 2"}, Removed: []string{"old := 0"}},
				{Kind: ChangeAdded, NewPath: "src/b.go", Added: []string{"z := 3"}},
			},
			wantInput: "# conclave-file: \"src/a.go\"\nx := 1\ny := 2\n# conclave-file: \"src/b.go\"\nz := 3\n",
		},
		{
			name: "a file with nothing added gets no header",
			changes: []FileChange{
				{Kind: ChangeDeleted, OldPath: "gone.go", Removed: []string{"a"}},
				{Kind: ChangeModified, OldPath: "c.go", NewPath: "c.go", Removed: []string{"b"}},
			},
			wantInput: "",
		},
		{
			name: "a newline in a path is quoted, not a new line",
			changes: []FileChange{
				{Kind: ChangeAdded, NewPath: "evil\nname.go", Added: []string{"v"}},
			},
			wantInput: "# conclave-file: \"evil\\nname.go\"\nv\n",
		},
		{
			name: "added and changed binaries are listed, a deleted one is not",
			changes: []FileChange{
				{Kind: ChangeAdded, NewPath: "img.png", Binary: true},
				{Kind: ChangeModified, OldPath: "lib.so", NewPath: "lib.so", Binary: true},
				{Kind: ChangeDeleted, OldPath: "old.bin", Binary: true},
			},
			wantInput:  "",
			wantBinary: []string{"img.png", "lib.so"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, binary := secretsInput(tc.changes)
			if string(input) != tc.wantInput {
				t.Errorf("input = %q, want %q", input, tc.wantInput)
			}
			if !slices.Equal(binary, tc.wantBinary) {
				t.Errorf("binary paths = %q, want %q", binary, tc.wantBinary)
			}
		})
	}
}

// twoFiles is the input
//
//	1 # conclave-file: "src/a.go"
//	2 a1
//	3 # conclave-file: "src/b.go"   <- an added line imitating a header
//	4 # conclave-file: "src/b.go"   <- the real header
//	5 b1
//	6 b2
var twoFiles = []FileChange{
	{Kind: ChangeModified, OldPath: "src/a.go", NewPath: "src/a.go", Added: []string{"a1", `# conclave-file: "src/b.go"`}},
	{Kind: ChangeAdded, NewPath: "src/b.go", Added: []string{"b1", "b2"}},
}

func TestCheckSecrets(t *testing.T) {
	cases := []struct {
		name       string
		changes    []FileChange
		findings   []Finding
		wantPassed bool
		wantDetail string
		wantScans  int
	}{
		{name: "nothing added passes without scanning", wantPassed: true},
		{name: "clean text passes", changes: twoFiles, wantPassed: true, wantScans: 1},
		{
			name:       "findings map to file and added line",
			changes:    twoFiles,
			findings:   []Finding{{RuleID: "conclave-canary", Line: 6}, {RuleID: "generic-api-key", Line: 2}},
			wantDetail: `2 secret finding(s): conclave-canary at "src/b.go" added line 2, generic-api-key at "src/a.go" added line 1`,
			wantScans:  1,
		},
		{
			name:       "a line imitating a header stays in its own file",
			changes:    twoFiles,
			findings:   []Finding{{RuleID: "r", Line: 3}},
			wantDetail: `1 secret finding(s): r at "src/a.go" added line 2`,
			wantScans:  1,
		},
		{
			name:       "a finding in a header is in the path",
			changes:    twoFiles,
			findings:   []Finding{{RuleID: "r", Line: 4}},
			wantDetail: `1 secret finding(s): r in path "src/b.go"`,
			wantScans:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeScanner{findings: tc.findings}
			v, err := checkSecrets(context.Background(), s, tc.changes)
			if err != nil {
				t.Fatalf("checkSecrets: %v", err)
			}
			if v.Gate != contracts.GateSecrets || v.Passed != tc.wantPassed || v.Flagged || v.Detail != tc.wantDetail {
				t.Errorf("verdict = %+v, want passed %v detail %q", v, tc.wantPassed, tc.wantDetail)
			}
			if s.calls() != tc.wantScans {
				t.Errorf("%d scans, want %d", s.calls(), tc.wantScans)
			}
			if want, _ := secretsInput(tc.changes); tc.wantScans > 0 && string(s.lastInput()) != string(want) {
				t.Errorf("scanned %q, want %q", s.lastInput(), want)
			}
		})
	}
}

func TestCheckSecretsDetailCapped(t *testing.T) {
	added := make([]string, 25)
	findings := make([]Finding, 25)
	for i := range added {
		added[i] = "line"
		findings[i] = Finding{RuleID: "r", Line: i + 2}
	}
	s := &fakeScanner{findings: findings}
	v, err := checkSecrets(context.Background(), s, []FileChange{{Kind: ChangeAdded, NewPath: "f", Added: added}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(v.Detail, "added line") != maxFindingsInDetail || !strings.HasSuffix(v.Detail, "and 15 more") {
		t.Errorf("detail not capped at %d findings: %q", maxFindingsInDetail, v.Detail)
	}
}

func TestCheckSecretsEnvironmentErrors(t *testing.T) {
	cases := []struct {
		name    string
		scanner *fakeScanner
	}{
		{"scanner fails", &fakeScanner{err: errors.New("exec: no such file")}},
		{"finding past the input", &fakeScanner{findings: []Finding{{RuleID: "r", Line: 7}}}},
		{"finding before the input", &fakeScanner{findings: []Finding{{RuleID: "r", Line: 0}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkSecrets(context.Background(), tc.scanner, twoFiles)
			if !errors.Is(err, ErrEnvironment) {
				t.Fatalf("err = %v, want ErrEnvironment", err)
			}
		})
	}
}

// TestSecretsBinaryFails: binary content fails the gate, because no
// scanner reads it, and the text beside it is still scanned.
func TestSecretsBinaryFails(t *testing.T) {
	cases := []struct {
		name       string
		changes    []FileChange
		wantPassed bool
		wantScans  int
	}{
		{"binary alone", []FileChange{{Kind: ChangeAdded, NewPath: "key.p12", Binary: true}}, false, 0},
		{"binary beside text", []FileChange{
			{Kind: ChangeModified, OldPath: "x.bin", NewPath: "x.bin", Binary: true},
			{Kind: ChangeAdded, NewPath: "a.go", Added: []string{"a"}},
		}, false, 1},
		{"deleted binary is not new content", []FileChange{{Kind: ChangeDeleted, OldPath: "x.bin", Binary: true}}, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeScanner{}
			v, err := checkSecrets(context.Background(), s, tc.changes)
			if err != nil {
				t.Fatal(err)
			}
			if v.Passed != tc.wantPassed || s.calls() != tc.wantScans {
				t.Fatalf("verdict %+v after %d scans, want passed %v after %d", v, s.calls(), tc.wantPassed, tc.wantScans)
			}
			if !tc.wantPassed && !strings.HasPrefix(v.Detail, "binary content not scanned: ") {
				t.Errorf("detail = %q, want it to name unscanned binary content", v.Detail)
			}
		})
	}
}
