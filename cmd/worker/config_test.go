package main

import (
	"strings"
	"testing"
)

func TestSameRepositories(t *testing.T) {
	tests := []struct {
		name    string
		byFile  map[string][]string
		wantErr []string
	}{
		{
			name:   "all agree",
			byFile: map[string][]string{"repos.yaml": {"a/b", "c/d"}, "gates.yaml": {"c/d", "a/b"}},
		},
		{
			name:    "one file lacks a repository",
			byFile:  map[string][]string{"repos.yaml": {"a/b", "c/d"}, "sandboxes.yaml": {"a/b"}},
			wantErr: []string{"sandboxes.yaml does not configure c/d"},
		},
		{
			name:   "each file lacks one",
			byFile: map[string][]string{"repos.yaml": {"a/b"}, "toolchains.yaml": {"c/d"}},
			wantErr: []string{
				"repos.yaml does not configure c/d", "toolchains.yaml does not configure a/b",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sameRepositories(tt.byFile)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tt.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

// TestLoadDeployRefusesShippedPlaceholders checks the worker will not
// start on the shipped deploy files: each carries placeholders that fail
// validation, and the error names the file.
func TestLoadDeployRefusesShippedPlaceholders(t *testing.T) {
	_, err := loadDeploy(paths{
		Router: "../../deploy/router.yaml", Repos: "../../deploy/repos.yaml", Toolchains: "../../deploy/toolchains.yaml",
		Sandboxes: "../../deploy/sandboxes.yaml", Gates: "../../deploy/gates.yaml", GitHub: "../../deploy/github.yaml",
	})
	if err == nil || !strings.Contains(err.Error(), "deploy/") {
		t.Fatalf("error = %v, want a refusal naming a deploy file", err)
	}
}

// TestLoadDeployMissingFile names the file it could not read.
func TestLoadDeployMissingFile(t *testing.T) {
	_, err := loadDeploy(paths{Router: "testdata/missing.yaml"})
	if err == nil || !strings.Contains(err.Error(), "testdata/missing.yaml") {
		t.Fatalf("error = %v", err)
	}
}
