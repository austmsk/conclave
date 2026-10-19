package github_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/austmsk/conclave/internal/github"
)

// parseCommandResult is what one case expects: not a command, a malformed
// command, or a parsed one.
type parseCommandResult int

const (
	cmdNotCommand parseCommandResult = iota
	cmdMalformed
	cmdParsed
)

var parseCommandCases = []struct {
	name string
	body string
	want parseCommandResult
	cmd  github.Command
}{
	{"approve bare", "/approve", cmdParsed, github.Command{Name: github.CommandApprove}},
	{"cancel bare", "/cancel", cmdParsed, github.Command{Name: github.CommandCancel}},
	{"status bare", "/status", cmdParsed, github.Command{Name: github.CommandStatus}},
	{"retry bare", "/retry", cmdParsed, github.Command{Name: github.CommandRetry}},
	{"help bare", "/help", cmdParsed, github.Command{Name: github.CommandHelp}},
	{"revise bare refused", "/revise", cmdMalformed, github.Command{}},
	{"ask bare refused", "/ask", cmdMalformed, github.Command{}},
	{"approve sequential", "/approve sequential", cmdParsed,
		github.Command{Name: github.CommandApprove, Mode: github.ApproveSequential}},
	{"approve parallel", "/approve parallel", cmdParsed,
		github.Command{Name: github.CommandApprove, Mode: github.ApproveParallel}},
	{"approve accept risk", "/approve --accept-risk", cmdParsed,
		github.Command{Name: github.CommandApprove, AcceptRisk: true}},
	{"approve mode then flag", "/approve parallel --accept-risk", cmdParsed,
		github.Command{Name: github.CommandApprove, Mode: github.ApproveParallel, AcceptRisk: true}},
	{"approve flag then mode", "/approve --accept-risk sequential", cmdParsed,
		github.Command{Name: github.CommandApprove, Mode: github.ApproveSequential, AcceptRisk: true}},
	{"approve two modes refused", "/approve sequential parallel", cmdMalformed, github.Command{}},
	{"approve repeated flag refused", "/approve --accept-risk --accept-risk", cmdMalformed, github.Command{}},
	{"approve unknown argument refused", "/approve now", cmdMalformed, github.Command{}},
	{"approve second line refused", "/approve\nthanks!", cmdMalformed, github.Command{}},
	{"approve trailing blank lines", "/approve\n\n", cmdParsed, github.Command{Name: github.CommandApprove}},
	{"revise same line", "/revise use a map", cmdParsed,
		github.Command{Name: github.CommandRevise, Text: "use a map"}},
	{"revise multi line", "/revise use a map\nand test it", cmdParsed,
		github.Command{Name: github.CommandRevise, Text: "use a map\nand test it"}},
	{"revise text on next line", "/revise\nuse a map", cmdParsed,
		github.Command{Name: github.CommandRevise, Text: "use a map"}},
	{"ask same line", "/ask why a map?", cmdParsed,
		github.Command{Name: github.CommandAsk, Text: "why a map?"}},
	{"ask whitespace only refused", "/ask  \t\n  \n", cmdMalformed, github.Command{}},
	{"cancel with arguments refused", "/cancel now", cmdMalformed, github.Command{}},
	{"status extra line refused", "/status\nplease", cmdMalformed, github.Command{}},
	{"crlf", "/revise a\r\nb\r\n", cmdParsed, github.Command{Name: github.CommandRevise, Text: "a\nb"}},
	{"crlf after bare word", "/status\r\n", cmdParsed, github.Command{Name: github.CommandStatus}},
	{"trailing spaces and tabs", "/approve parallel \t ", cmdParsed,
		github.Command{Name: github.CommandApprove, Mode: github.ApproveParallel}},
	{"leading space", " /approve", cmdNotCommand, github.Command{}},
	{"backticks", "`/approve`", cmdNotCommand, github.Command{}},
	{"quote", "> /approve", cmdNotCommand, github.Command{}},
	{"code fence", "```\n/approve\n```", cmdNotCommand, github.Command{}},
	{"command on line two", "looks good\n/approve", cmdNotCommand, github.Command{}},
	{"capitalised word", "/Approve", cmdNotCommand, github.Command{}},
	{"longer word", "/approved", cmdNotCommand, github.Command{}},
	{"word with suffix", "/approvex", cmdNotCommand, github.Command{}},
	{"full-width slash", "\uFF0Fapprove", cmdNotCommand, github.Command{}},
	{"zero-width space prefix", "\u200B/approve", cmdNotCommand, github.Command{}},
	{"nbsp after word", "/approve\u00A0parallel", cmdNotCommand, github.Command{}},
	{"path", "/tmp/x", cmdNotCommand, github.Command{}},
	{"empty body", "", cmdNotCommand, github.Command{}},
}

func TestParseCommand(t *testing.T) {
	for _, tc := range parseCommandCases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, ok, err := github.ParseCommand(tc.body)
			switch tc.want {
			case cmdNotCommand:
				if ok || err != nil {
					t.Errorf("ParseCommand(%q) = ok %v, err %v; want not a command", tc.body, ok, err)
				}
			case cmdMalformed:
				if !ok || !errors.Is(err, github.ErrMalformedCommand) {
					t.Errorf("ParseCommand(%q) = ok %v, err %v; want ErrMalformedCommand", tc.body, ok, err)
				}
			case cmdParsed:
				if !ok || err != nil || cmd != tc.cmd {
					t.Errorf("ParseCommand(%q) = %+v, ok %v, err %v; want %+v", tc.body, cmd, ok, err, tc.cmd)
				}
			}
		})
	}
}

func FuzzParseCommand(f *testing.F) {
	for _, tc := range parseCommandCases {
		f.Add(tc.body)
	}
	f.Fuzz(func(t *testing.T, body string) {
		cmd, ok, err := github.ParseCommand(body)
		if err != nil && (!ok || !errors.Is(err, github.ErrMalformedCommand)) {
			t.Fatalf("ParseCommand(%q): ok %v with error %v", body, ok, err)
		}
		if !ok {
			return
		}
		first, _, _ := strings.Cut(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
		if !strings.HasPrefix(first, "/"+string(cmd.Name)) {
			t.Fatalf("ParseCommand(%q) read /%s from first line %q", body, cmd.Name, first)
		}
	})
}
