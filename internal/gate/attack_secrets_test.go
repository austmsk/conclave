package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// permissiveConfig would silence every rule if gitleaks ever loaded it.
const permissiveConfig = `title = "allow everything"
[allowlist]
regexes = ['''.*''']
paths = ['''.*''']
`

// plantedCanary is a canary on the second added line of src/b.go, input
// line 6 of the scan.
func plantedCanary(canary, suffix string) []FileChange {
	return []FileChange{
		twoFiles[0],
		{Kind: ChangeAdded, NewPath: "src/b.go", Added: []string{"b1", `key := "` + canary + `"` + suffix}},
	}
}

// requireCanaryFound checks that a real scan failed the gate on the
// planted canary and kept the canary itself out of the verdict.
func requireCanaryFound(t *testing.T, g SecretScanner, changes []FileChange, canary string) {
	t.Helper()
	v, err := checkSecrets(context.Background(), g, changes)
	if err != nil {
		t.Fatalf("checkSecrets: %v", err)
	}
	if v.Passed || !strings.Contains(v.Detail, `conclave-canary at "src/b.go" added line 2`) {
		t.Fatalf("verdict %+v: the planted canary was not found", v)
	}
	if strings.Contains(fmt.Sprintf("%+v", v), canary) {
		t.Fatalf("verdict holds the canary: %+v", v)
	}
}

// TestAttackGitleaksAllowIgnored: an agent appends "gitleaks:allow" to the
// line holding a secret. The worker passes --ignore-gitleaks-allow, so the
// comment is text like any other.
func TestAttackGitleaksAllowIgnored(t *testing.T) {
	canary := "canary_" + "allow_comment_q9"
	changes := plantedCanary(canary, " // gitleaks:allow")
	t.Run("flag passed", func(t *testing.T) {
		f := newFakeGitleaks(t, fakeBehaviour{exit: 99, report: `[{"RuleID":"conclave-canary","StartLine":6}]`})
		requireCanaryFound(t, f.start(t), changes, canary)
		args := strings.Split(f.recorded(t, "args"), "\n")
		if !slices.Contains(args, "--ignore-gitleaks-allow") {
			t.Errorf("gitleaks ran without --ignore-gitleaks-allow: %q", args)
		}
	})
	t.Run("real", func(t *testing.T) {
		requireCanaryFound(t, realGitleaks(t), changes, canary)
	})
}

// plantRepoConfig sets up everything a repository or a poisoned worker
// environment could use to replace the platform config: GITLEAKS_CONFIG,
// GITLEAKS_CONFIG_TOML, and a .gitleaks.toml and .gitleaksignore in the
// worker's working directory. It returns a variable value that must never
// reach the scanner's environment.
func plantRepoConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	permissive := filepath.Join(dir, ".gitleaks.toml")
	if err := os.WriteFile(permissive, []byte(permissiveConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	// Fingerprints of stdin findings are ":<rule>:<line>"; cover every
	// line the test inputs have.
	var ignore strings.Builder
	for line := 1; line <= 64; line++ {
		fmt.Fprintf(&ignore, ":conclave-canary:%d\n:generic-api-key:%d\n", line, line)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitleaksignore"), []byte(ignore.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITLEAKS_CONFIG", permissive)
	t.Setenv("GITLEAKS_CONFIG_TOML", permissiveConfig)
	leak := "canary_" + "worker_env_x2"
	t.Setenv("CONCLAVE_TEST_WORKER_SECRET", leak)
	t.Chdir(dir)
	return leak
}

// TestAttackRepoGitleaksConfigIgnored: a repository ships a .gitleaks.toml
// or .gitleaksignore that allows everything, or the worker's environment
// names one. The scan runs with an empty environment, from an empty
// directory, with the private copy of the platform config.
func TestAttackRepoGitleaksConfigIgnored(t *testing.T) {
	canary := "canary_" + "repo_config_m4"
	repoConfig := FileChange{Kind: ChangeAdded, NewPath: ".gitleaks.toml", Added: strings.Split(permissiveConfig, "\n")}
	changes := append([]FileChange{repoConfig}, plantedCanary(canary, "")...)

	t.Run("isolated", func(t *testing.T) {
		leak := plantRepoConfig(t)
		f := newFakeGitleaks(t, fakeBehaviour{report: "[]"})
		g := f.start(t)
		if _, err := checkSecrets(context.Background(), g, changes); err != nil {
			t.Fatal(err)
		}
		checkIsolated(t, f, g, leak)
	})
	t.Run("real", func(t *testing.T) {
		g := realGitleaks(t)
		plantRepoConfig(t)
		v, err := checkSecrets(context.Background(), g, changes)
		if err != nil {
			t.Fatal(err)
		}
		if v.Passed || !strings.Contains(v.Detail, "conclave-canary") {
			t.Fatalf("verdict %+v: the planted canary was not found", v)
		}
	})
}

// checkIsolated checks what the fake recorded: no inherited environment,
// an empty working directory of the scan's own, and the platform config's
// private copy.
func checkIsolated(t *testing.T, f fakeGitleaks, g *Gitleaks, leak string) {
	t.Helper()
	env := f.recorded(t, "env")
	for _, bad := range []string{"GITLEAKS_", leak, "HOME=" + os.Getenv("HOME")} {
		if strings.Contains(env, bad) {
			t.Errorf("scanner environment contains %q:\n%s", bad, env)
		}
	}
	if pwd := strings.TrimSpace(f.recorded(t, "pwd")); filepath.Base(pwd) != "cwd" {
		t.Errorf("scanner ran in %q, not its own empty directory", pwd)
	}
	if ls := f.recorded(t, "ls"); ls != "" {
		t.Errorf("scanner working directory was not empty: %q", ls)
	}
	args := strings.Split(f.recorded(t, "args"), "\n")
	i := slices.Index(args, "--config")
	if i < 0 || i+1 >= len(args) || args[i+1] != g.config {
		t.Fatalf("scanner not given the private config copy %s: %q", g.config, args)
	}
	got, err := os.ReadFile(g.config)
	if err != nil || string(got) != "title = \"fake\"\n" {
		t.Errorf("private config holds %q, %v; want the platform config", got, err)
	}
}

// TestSecretsEvidenceHasNoCanary: nothing the secrets gate returns, on
// success or failure, holds the matched text, even when the report carries
// it in every field gitleaks has.
func TestSecretsEvidenceHasNoCanary(t *testing.T) {
	canary := "canary_" + "evidence_z8"
	changes := plantedCanary(canary, "")
	leaky := `[{"RuleID":"conclave-canary","StartLine":6,"Match":"` + canary + `","Secret":"` + canary +
		`","Line":"key := \"` + canary + `\"","Fingerprint":"` + canary + `"}]`

	t.Run("findings", func(t *testing.T) {
		g := newFakeGitleaks(t, fakeBehaviour{exit: 99, report: leaky}).start(t)
		found, err := g.Scan(context.Background(), []byte("a\nb\nc\nd\ne\nf\n"))
		if err != nil {
			t.Fatal(err)
		}
		if s := fmt.Sprintf("%+v", found); strings.Contains(s, canary) {
			t.Errorf("findings hold the canary: %s", s)
		}
		requireCanaryFound(t, g, changes, canary)
	})
	t.Run("error", func(t *testing.T) {
		g := newFakeGitleaks(t, fakeBehaviour{exit: 0, report: leaky}).start(t)
		_, err := checkSecrets(context.Background(), g, changes)
		if err == nil || strings.Contains(err.Error(), canary) {
			t.Errorf("err = %v: want an error without the canary", err)
		}
	})
	t.Run("real", func(t *testing.T) {
		requireCanaryFound(t, realGitleaks(t), changes, canary)
	})
}
