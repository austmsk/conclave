package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfirmID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  error
	}{
		{"the ID", "12\n", nil},
		{"the ID with spaces", "  12 \n", nil},
		{"the ID then end of input", "12", nil},
		{"empty, as from /dev/null", "", errNotConfirmed},
		{"another halt's ID", "13\n", errNotConfirmed},
		{"yes", "yes\n", errNotConfirmed},
		{"the ID twice", "1212\n", errNotConfirmed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var prompt strings.Builder
			err := confirmID(strings.NewReader(tt.input), &prompt, 12)
			if !errors.Is(err, tt.want) {
				t.Fatalf("confirmID(%q) = %v, want %v", tt.input, err, tt.want)
			}
			if !strings.Contains(prompt.String(), "Type 12 to lift halt 12") {
				t.Fatalf("prompt %q does not name the halt", prompt.String())
			}
		})
	}
}

// TestAttackScriptedResumeRefused: input piped or redirected from a file,
// as a script or an agent's shell would supply it, is refused before the
// confirmation is even read, though it carries the right ID.
func TestAttackScriptedResumeRefused(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	_, _ = io.WriteString(w, "3\n")
	path := filepath.Join(t.TempDir(), "answer")
	if err := os.WriteFile(path, []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	for name, in := range map[string]*os.File{"pipe": r, "file": f} {
		t.Run(name, func(t *testing.T) {
			if err := terminalConfirm(in, io.Discard)(3); !errors.Is(err, errNoTerminal) {
				t.Fatalf("confirm from a %s = %v, want errNoTerminal", name, err)
			}
		})
	}
}
