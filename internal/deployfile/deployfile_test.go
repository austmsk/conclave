package deployfile_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/deployfile"
)

var errInvalid = errors.New("invalid test configuration")

type file struct {
	Repositories map[string]struct {
		Value int `yaml:"value"`
	} `yaml:"repositories"`
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name, yaml, errPart string
	}{
		{name: "valid", yaml: "repositories:\n  a/b:\n    value: 1\n"},
		{name: "unknown key", yaml: "repositories:\n  a/b:\n    valu: 1\n", errPart: "parsing YAML"},
		{name: "empty", yaml: "", errPart: "parsing YAML"},
		{name: "two documents", yaml: "repositories: {}\n---\nrepositories: {}\n", errPart: "more than one YAML document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f file
			err := deployfile.Decode(strings.NewReader(tt.yaml), &f, errInvalid)
			if tt.errPart == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, errInvalid) || !strings.Contains(err.Error(), tt.errPart) {
				t.Fatalf("got %v, want %q wrapping the sentinel", err, tt.errPart)
			}
		})
	}
}

func TestRepoKeys(t *testing.T) {
	tests := []struct {
		name    string
		keys    []string
		want    string
		errPart string
	}{
		{name: "sorted", keys: []string{"b/x", "a/y"}, want: "a/y,b/x"},
		{name: "placeholder", keys: []string{"owner/repo"}, errPart: `repositories."owner/repo": placeholder`},
		{name: "no owner", keys: []string{"tally"}, errPart: "not a repository full name"},
		{name: "dot name", keys: []string{"a/.."}, errPart: "not a valid repository name"},
		{name: "case duplicate", keys: []string{"a/b", "A/B"}, errPart: `repositories."a/b": same repository as "A/B"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]int{}
			for _, k := range tt.keys {
				m[k] = 1
			}
			got, err := deployfile.RepoKeys(m)
			if tt.errPart != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errPart) {
					t.Fatalf("got %v, want %q", err, tt.errPart)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != tt.want {
				t.Fatalf("got %v, %v, want %s", got, err, tt.want)
			}
		})
	}
}

// TestLoadHashesWhatWasLoaded checks the digest is of the bytes handed to
// the loader, and that a refused file returns no digest.
func TestLoadHashesWhatWasLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.yaml")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen string
	digest, err := deployfile.Load(path, func(r io.Reader) error {
		b, err := io.ReadAll(r)
		seen = string(b)
		return err
	})
	// SHA-256 of "abc".
	if err != nil || seen != "abc" || digest != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("got %q, %q, %v", digest, seen, err)
	}
	digest, err = deployfile.Load(path, func(io.Reader) error { return errInvalid })
	if !errors.Is(err, errInvalid) || digest != "" || !strings.Contains(err.Error(), path) {
		t.Fatalf("refused load: got %q, %v", digest, err)
	}
	if _, err := deployfile.Load(filepath.Join(t.TempDir(), "missing.yaml"), nil); err == nil {
		t.Fatal("missing file accepted")
	}
}
