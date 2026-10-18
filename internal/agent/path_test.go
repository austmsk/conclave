package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/austmsk/conclave/internal/agent/agenttest"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

// fileToolEnv is the configuration the file tools see in tests.
func fileToolEnv(denylist ...string) toolEnv {
	return toolEnv{denylist: denylist, maxCommandTimeout: 30 * time.Second}
}

// toolArgs encodes a tool's arguments.
func toolArgs(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// execOnHost makes sb run commands on the host in its directory, so the
// exec-based tools' scripts and parsing can be tested without Docker. The
// security tests run against the real sandbox instead.
func execOnHost(sb *agenttest.Sandbox) {
	sb.ExecFunc = func(ctx context.Context, req contracts.ExecRequest) (contracts.ExecResult, error) {
		cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
		cmd.Dir = sb.Dir
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return contracts.ExecResult{}, err
		}
		return contracts.ExecResult{ExitCode: cmd.ProcessState.ExitCode(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
	}
}

// seed writes files under dir, creating parents.
func seed(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDenylistComponentAware(t *testing.T) {
	cases := []struct {
		name     string
		denylist []string
		path     string
		denied   bool
	}{
		{"exact component", []string{"secrets"}, "secrets", true},
		{"under component", []string{"secrets"}, "secrets/key.pem", true},
		{"nested component", []string{"secrets"}, "app/secrets/key.pem", true},
		{"longer name", []string{"secrets"}, "secretsfoo", false},
		{"longer name dir", []string{"secrets"}, "secretsfoo/key.pem", false},
		{"prefixed name", []string{"secrets"}, "foosecrets/key.pem", false},
		{"extension", []string{"secrets"}, "secrets.md", false},
		{"multi exact", []string{"config/prod.yaml"}, "config/prod.yaml", true},
		{"multi nested", []string{"config/prod.yaml"}, "deploy/config/prod.yaml", true},
		{"multi longer name", []string{"config/prod.yaml"}, "config/prod.yaml.example", false},
		{"multi sibling", []string{"config/prod.yaml"}, "config/dev.yaml", false},
		{"multi last component only", []string{"config/prod.yaml"}, "prod.yaml", false},
		{"multi split", []string{"config/prod.yaml"}, "config/x/prod.yaml", false},
		{"multi parent only", []string{"config/prod.yaml"}, "config", false},
		{"upper case", []string{"secrets"}, "Secrets/key.pem", true},
		{"entry upper case", []string{"Secrets"}, "secrets/key.pem", true},
		{"trailing dot", []string{"secrets"}, "secrets./key.pem", true},
		{"trailing space", []string{"secrets"}, "secrets /key.pem", true},
		{"hfs ignorable", []string{"secrets"}, "sec\U0000200crets/key.pem", true},
		{"ntfs stream", []string{"secrets"}, "secrets::$DATA", true},
		{"entry with slashes", []string{"/secrets/"}, "secrets/key.pem", true},
		{"empty entry ignored", []string{""}, "src/main.go", false},
		{"glob is literal", []string{"*.pem"}, "key.pem", false},
		{"second entry", []string{"vendor", "secrets"}, "secrets/key.pem", true},
		{"no denylist", nil, "secrets/key.pem", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPath(tc.path, tc.denylist)
			if !tc.denied {
				if err != nil {
					t.Fatalf("checkPath(%q, %q) = %v, want allowed", tc.path, tc.denylist, err)
				}
				return
			}
			if !errors.Is(err, sandbox.ErrInvalidPath) {
				t.Fatalf("checkPath(%q, %q) = %v, want ErrInvalidPath", tc.path, tc.denylist, err)
			}
		})
	}
}

func TestCheckPathAppliesSandboxRules(t *testing.T) {
	for _, p := range []string{"../x", "/etc/passwd", ".git/config", ".GIT/config", "git~1/config", "a/\U0000202ex"} {
		if err := checkPath(p, nil); !errors.Is(err, sandbox.ErrInvalidPath) {
			t.Errorf("checkPath(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestDecodeArgsStrict(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"unknown field", `{"path":"a","mode":"x"}`},
		{"trailing data", `{"path":"a"} {}`},
		{"not an object", `["a"]`},
		{"wrong type", `{"path":1}`},
		{"empty", ``},
		{"invalid json", `{"path":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a readFileArgs
			err := decodeArgs(json.RawMessage(tc.raw), &a)
			var r *Refusal
			if !errors.As(err, &r) || r.Code != codeBadArgs || r.Rejected {
				t.Fatalf("decodeArgs(%s) = %v, want an invalid_arguments refusal", tc.raw, err)
			}
		})
	}
}

// TestAttackSiblingPrefix proves a path naming a sibling of the workspace
// whose name starts with the workspace's own (the "<root>-evil" escape a
// prefix check misses) is refused by the tool's own CheckPath: the
// sandbox, which would also refuse it, is never called.
func TestAttackSiblingPrefix(t *testing.T) {
	for _, tool := range []contracts.Tool{
		newReadFile(fileToolEnv()), newWriteFile(fileToolEnv()), newListDir(fileToolEnv()), newSearch(fileToolEnv()),
	} {
		t.Run(string(tool.Name()), func(t *testing.T) {
			probe := agenttest.NewSandbox(t)
			base := filepath.Base(probe.Dir)
			sibling := probe.Dir + "-evil"
			seed(t, sibling, map[string]string{"x": "canary_sibling"})
			for _, p := range []string{
				"../" + base + "-evil/x",
				"../" + base + "-evil",
				sibling + "/x",
				"./../" + base + "-evil/x",
				"src/../../" + base + "-evil/x",
				"%2e%2e/" + base + "-evil/x",
				"....//" + base + "-evil/x",
				"..\\" + base + "-evil\\x",
			} {
				sb := agenttest.NewSandbox(t)
				execOnHost(sb)
				_, err := tool.Execute(context.Background(), sb, siblingArgs(t, tool.Name(), p))
				if !errors.Is(err, sandbox.ErrInvalidPath) || !classify(err).rejected {
					t.Errorf("%s(%q) = %v, want a rejection wrapping ErrInvalidPath", tool.Name(), p, err)
				}
				if calls := sb.Calls(); len(calls) != 0 {
					t.Errorf("%s(%q) reached the sandbox: %+v", tool.Name(), p, calls)
				}
			}
			if data, err := os.ReadFile(filepath.Join(sibling, "x")); err != nil || string(data) != "canary_sibling" {
				t.Errorf("sibling file changed: %q, %v", data, err)
			}
		})
	}
}

func siblingArgs(t *testing.T, tool contracts.ToolName, p string) json.RawMessage {
	switch tool {
	case contracts.ToolWriteFile:
		return toolArgs(t, map[string]any{"path": p, "content": "pwned"})
	case contracts.ToolSearch:
		return toolArgs(t, map[string]any{"pattern": "canary", "path": p})
	default:
		return toolArgs(t, map[string]any{"path": p})
	}
}
