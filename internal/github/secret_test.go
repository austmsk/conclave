package github_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/github"
)

// Token-shaped samples are assembled from parts so no literal credential
// prefix appears in the source; the bodies are canaries.
const secretTokenBody = "canary_0123456789abcdefghijklmnopqrstuv"

var (
	secretInstallationToken = "gh" + "s_" + secretTokenBody
	secretFineGrainedToken  = "github" + "_pat_" + secretTokenBody
	secretJWT               = "eyJ" + "hbGciOiJSUzI1NiJ9" + ".eyJpc3MiOiJjYW5hcnkifQ" + ".canary_signature_0123456789"
)

var redactCases = []struct {
	name  string
	in    string
	known []string
	want  string
}{
	{"personal token", "token " + "gh" + "p_" + secretTokenBody + " end", nil, "token [redacted] end"},
	{"oauth token", "token " + "gh" + "o_" + secretTokenBody + " end", nil, "token [redacted] end"},
	{"user-to-server token", "token " + "gh" + "u_" + secretTokenBody + " end", nil, "token [redacted] end"},
	{"installation token", "token " + secretInstallationToken + " end", nil, "token [redacted] end"},
	{"refresh token", "token " + "gh" + "r_" + secretTokenBody + " end", nil, "token [redacted] end"},
	{"fine-grained token", "pat=" + secretFineGrainedToken, nil, "pat=[redacted]"},
	{"jwt", "Bearer " + secretJWT, nil, "Bearer [redacted]"},
	{"token in a url", "https://x-access-token:" + secretInstallationToken + "@github.com/o/r",
		nil, "https://x-access-token:[redacted]@github.com/o/r"},
	{"token after percent-encoding", "q=token%3D" + secretInstallationToken, nil, "q=token%3D[redacted]"},
	{"token after an escaped newline", `{"m":"bad\n` + secretInstallationToken + `"}`, nil, `{"m":"bad\n[redacted]"}`},
	{"jwt after a word character", "Bearer%20" + secretJWT, nil, "Bearer%20[redacted]"},
	{"known canary value", "key canary_webhook_secret_v1 here", []string{"canary_webhook_secret_v1"}, "key [redacted] here"},
	{"empty known value ignored", "plain text", []string{""}, "plain text"},
	{"short prefix-alike unchanged", "gh" + "s_short", nil, "gh" + "s_short"},
	{"prose unchanged", "pushed agent/12/integration at 1f6a1d3", nil, "pushed agent/12/integration at 1f6a1d3"},
}

func TestRedact(t *testing.T) {
	for _, tc := range redactCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := github.Redact(tc.in, tc.known...); got != tc.want {
				t.Errorf("Redact = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRefuseSecrets(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		known   []string
		refused bool
	}{
		{"clean body", "Plan approved; opening agent/12/integration.", nil, false},
		{"token-shaped string", "debug: " + secretInstallationToken, nil, true},
		{"known secret", "the secret is canary_webhook_secret_v1", []string{"canary_webhook_secret_v1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := github.RefuseSecrets(tc.body, tc.known...)
			if refused := errors.Is(err, github.ErrRefused); refused != tc.refused {
				t.Errorf("RefuseSecrets = %v, want refused %v", err, tc.refused)
			}
		})
	}
}

func TestRedactAttr(t *testing.T) {
	cases := []struct {
		name string
		log  func(*slog.Logger)
	}{
		{"message", func(l *slog.Logger) { l.Info("pushing with " + secretInstallationToken) }},
		{"string attr", func(l *slog.Logger) { l.Info("push", "header", "Bearer "+secretJWT) }},
		{"error attr", func(l *slog.Logger) { l.Info("push", "err", fmt.Errorf("auth failed: %s", secretFineGrainedToken)) }},
		{"attr in a group", func(l *slog.Logger) { l.WithGroup("git").Info("push", "stderr", secretInstallationToken) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.log(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: github.RedactAttr})))
			out := buf.String()
			if strings.Contains(out, secretTokenBody) || strings.Contains(out, "eyJ") {
				t.Errorf("log line leaks a token: %s", out)
			}
			if !strings.Contains(out, "[redacted]") {
				t.Errorf("log line has no [redacted] marker: %s", out)
			}
		})
	}
}

// secretHolder carries a secret in an exported field. Formatting reaches
// the secret's own methods only through exported fields: a secret in an
// unexported field is printed by reflection, so %+v of a struct holding
// one shows the raw value. That is why App, which holds its secrets in
// unexported fields, has its own Format rather than relying on the secret
// type's.
type secretHolder struct{ Token any }

func TestSecretFormatsRedacted(t *testing.T) {
	const value = "canary_installation_token_value"
	s := github.NewSecret(value)
	cases := []struct {
		name   string
		format func() string
	}{
		{"%v", func() string { return fmt.Sprintf("%v", s) }},
		{"%+v", func() string { return fmt.Sprintf("%+v", s) }},
		{"%#v", func() string { return fmt.Sprintf("%#v", s) }},
		{"%s", func() string { return fmt.Sprintf("%s", s) }},
		{"%q", func() string { return fmt.Sprintf("%q", s) }},
		{"exported field %+v", func() string { return fmt.Sprintf("%+v", secretHolder{s}) }},
		{"exported field %#v", func() string { return fmt.Sprintf("%#v", secretHolder{s}) }},
		{"slog", func() string { return secretLogged(slog.Any("token", s)) }},
		{"slog in a group", func() string { return secretLogged(slog.Group("app", slog.Any("token", s))) }},
		{"json", func() string { return secretJSON(t, s) }},
		{"json in a struct", func() string { return secretJSON(t, secretHolder{s}) }},
		{"json map value", func() string { return secretJSON(t, map[string]any{"token": s}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.format()
			if strings.Contains(got, value) || !strings.Contains(got, "[redacted]") {
				t.Errorf("formatted secret = %q, want [redacted] and no value", got)
			}
		})
	}
}

func secretLogged(a slog.Attr) string {
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("event", a)
	return buf.String()
}

func secretJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}
