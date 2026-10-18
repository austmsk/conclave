package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/sandbox"
)

func TestReadFileRanges(t *testing.T) {
	const five = "one\ntwo\nthree\nfour\nfive\n"
	cases := []struct {
		name    string
		content string
		args    map[string]any
		want    string
	}{
		{"whole file", five, map[string]any{}, five},
		{"middle range", five, map[string]any{"start_line": 2, "end_line": 3}, "two\nthree\n"},
		{"single line", five, map[string]any{"start_line": 4, "end_line": 4}, "four\n"},
		{"start only", five, map[string]any{"start_line": 4}, "four\nfive\n"},
		{"end only", five, map[string]any{"end_line": 2}, "one\ntwo\n"},
		{"end past eof", five, map[string]any{"start_line": 5, "end_line": 99}, "five\n"},
		{"no final newline", "a\nb", map[string]any{"start_line": 2}, "b"},
		{"crlf kept", "a\r\nb\r\n", map[string]any{"start_line": 1, "end_line": 1}, "a\r\n"},
		{"empty file whole", "", map[string]any{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			seed(t, sb.Dir, map[string]string{"src/f.txt": tc.content})
			tc.args["path"] = "src/f.txt"
			res, err := newReadFile(fileToolEnv()).Execute(context.Background(), sb, toolArgs(t, tc.args))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Output != tc.want {
				t.Fatalf("output = %q, want %q", res.Output, tc.want)
			}
		})
	}
}

// TestReadFileRefusals covers arguments refused before the sandbox is
// called: each is a refusal the model can correct, and none reaches it.
func TestReadFileRefusals(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		code string
	}{
		{"missing path", map[string]any{}, codeBadArgs},
		{"unknown field", map[string]any{"path": "a.txt", "lines": "1-2"}, codeBadArgs},
		{"zero start", map[string]any{"path": "a.txt", "start_line": 0}, codeBadRange},
		{"negative end", map[string]any{"path": "a.txt", "end_line": -1}, codeBadRange},
		{"end before start", map[string]any{"path": "a.txt", "start_line": 3, "end_line": 2}, codeBadRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := agenttest.NewSandbox(t)
			_, err := newReadFile(fileToolEnv()).Execute(context.Background(), sb, toolArgs(t, tc.args))
			var r *Refusal
			if !errors.As(err, &r) || r.Code != tc.code {
				t.Fatalf("Execute = %v, want refusal %s", err, tc.code)
			}
			if calls := sb.Calls(); len(calls) != 0 {
				t.Fatalf("refused call reached the sandbox: %+v", calls)
			}
		})
	}
}

func TestReadFileStartPastEnd(t *testing.T) {
	sb := agenttest.NewSandbox(t)
	seed(t, sb.Dir, map[string]string{"a.txt": "one\ntwo\n"})
	_, err := newReadFile(fileToolEnv()).Execute(context.Background(), sb, toolArgs(t, map[string]any{"path": "a.txt", "start_line": 3}))
	var r *Refusal
	if !errors.As(err, &r) || r.Code != codeBadRange {
		t.Fatalf("Execute = %v, want refusal %s", err, codeBadRange)
	}
}

func TestReadFileDeniedPathNeverReachesSandbox(t *testing.T) {
	for _, p := range []string{"secrets/key.pem", "app/Secrets/key.pem", ".git/config", ".GIT/config", "git~1/HEAD"} {
		sb := agenttest.NewSandbox(t)
		seed(t, sb.Dir, map[string]string{"secrets/key.pem": "canary_key"})
		res, err := newReadFile(fileToolEnv("secrets")).Execute(context.Background(), sb, toolArgs(t, map[string]any{"path": p}))
		if !errors.Is(err, sandbox.ErrInvalidPath) {
			t.Errorf("read_file(%q) = %q, %v; want ErrInvalidPath", p, res.Output, err)
		}
		if calls := sb.Calls(); len(calls) != 0 {
			t.Errorf("read_file(%q) reached the sandbox: %+v", p, calls)
		}
	}
}

func TestReadFileSandboxRefusalsPassThrough(t *testing.T) {
	sb := agenttest.NewSandbox(t)
	seed(t, sb.Dir, map[string]string{"dir/a.txt": "x"})
	cases := []struct {
		path string
		want error
	}{
		{"missing.txt", sandbox.ErrNotFile},
		{"dir", sandbox.ErrNotFile},
	}
	for _, tc := range cases {
		_, err := newReadFile(fileToolEnv()).Execute(context.Background(), sb, toolArgs(t, map[string]any{"path": tc.path}))
		if !errors.Is(err, tc.want) {
			t.Errorf("read_file(%q) = %v, want %v", tc.path, err, tc.want)
		}
	}
}
