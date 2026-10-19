package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/sandbox"
)

func TestWriteFileWrites(t *testing.T) {
	cases := []struct{ name, path, content string }{
		{"new file", "a.txt", "hello\n"},
		{"new parents", "src/deep/b.go", "package deep\n"},
		{"empty content", "empty.txt", ""},
		{"replaces", "existing.txt", "new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			seed(t, sb.Dir, map[string]string{"existing.txt": "old content"})
			res, err := newWriteFile(fileToolEnv()).Execute(context.Background(), sb, toolArgs(t, map[string]any{"path": tc.path, "content": tc.content}))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Output == "" {
				t.Error("no confirmation in the output")
			}
			got, err := os.ReadFile(filepath.Join(sb.Dir, filepath.FromSlash(tc.path)))
			if err != nil || string(got) != tc.content {
				t.Fatalf("file holds %q, %v; want %q", got, err, tc.content)
			}
		})
	}
}

func TestWriteFileRefusals(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing path", map[string]any{"content": "x"}},
		{"missing content", map[string]any{"path": "a.txt"}},
		{"null content", map[string]any{"path": "a.txt", "content": nil}},
		{"unknown field", map[string]any{"path": "a.txt", "content": "x", "mode": "0755"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			_, err := newWriteFile(fileToolEnv()).Execute(context.Background(), sb, toolArgs(t, tc.args))
			var r *Refusal
			if !errors.As(err, &r) || r.Code != codeBadArgs {
				t.Fatalf("Execute = %v, want refusal %s", err, codeBadArgs)
			}
			if calls := sb.Calls(); len(calls) != 0 {
				t.Fatalf("refused call reached the sandbox: %+v", calls)
			}
		})
	}
}

func TestWriteFileDeniedPathNeverReachesSandbox(t *testing.T) {
	for _, p := range []string{"secrets/key.pem", "SECRETS/new.pem", ".git/hooks/pre-commit", ".Git./config", "../escape.txt"} {
		sb := agenttest.NewSandbox(t)
		_, err := newWriteFile(fileToolEnv("secrets")).Execute(context.Background(), sb, toolArgs(t, map[string]any{"path": p, "content": "pwned"}))
		if !errors.Is(err, sandbox.ErrInvalidPath) {
			t.Errorf("write_file(%q) = %v, want ErrInvalidPath", p, err)
		}
		if calls := sb.Calls(); len(calls) != 0 {
			t.Errorf("write_file(%q) reached the sandbox: %+v", p, calls)
		}
	}
}
