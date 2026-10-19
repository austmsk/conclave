package github

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// CommandName is a slash command the owner can issue on an issue.
type CommandName string

const (
	CommandApprove CommandName = "approve"
	CommandRevise  CommandName = "revise"
	CommandAsk     CommandName = "ask"
	CommandCancel  CommandName = "cancel"
	CommandStatus  CommandName = "status"
	CommandRetry   CommandName = "retry"
	CommandHelp    CommandName = "help"
)

// ApproveMode is the execution mode /approve may name (FR-S13).
type ApproveMode string

const (
	ApproveDefault    ApproveMode = ""
	ApproveSequential ApproveMode = "sequential"
	ApproveParallel   ApproveMode = "parallel"
)

// Command is a parsed slash command.
type Command struct {
	Name CommandName
	// Mode and AcceptRisk are set only for /approve.
	Mode       ApproveMode
	AcceptRisk bool
	// Text is the feedback for /revise or the question for /ask.
	Text string
}

// ErrMalformedCommand reports a comment that starts with a command word
// but whose arguments do not fit it.
var ErrMalformedCommand = errors.New("malformed command")

// ParseCommand reads a comment body. ok reports whether the body is
// command-shaped: its first line starts at column 0 with a known command
// word. A command-shaped body with bad arguments returns ok and an error
// wrapping ErrMalformedCommand. Anything else, including a command in
// backticks, a quote, a code block, or indented, is not a command. Only
// the first line is ever read for a command word, so one comment carries
// at most one command, and platform text, which mentions commands only in
// backticks (NFR-U2a), never reads as one.
func ParseCommand(body string) (cmd Command, ok bool, err error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	first, rest, _ := strings.Cut(body, "\n")
	m := commandLine.FindStringSubmatch(first)
	if m == nil {
		return Command{}, false, nil
	}
	cmd = Command{Name: CommandName(m[1])}
	args, rest := m[2], strings.TrimSpace(rest)
	switch cmd.Name {
	case CommandApprove:
		if rest != "" {
			return cmd, true, fmt.Errorf("%w: /approve takes one line", ErrMalformedCommand)
		}
		cmd, err = parseApprove(cmd, args)
		return cmd, true, err
	case CommandRevise, CommandAsk:
		cmd.Text = strings.TrimSpace(args + "\n" + rest)
		if cmd.Text == "" {
			return cmd, true, fmt.Errorf("%w: /%s needs text", ErrMalformedCommand, cmd.Name)
		}
		return cmd, true, nil
	default:
		if args != "" || rest != "" {
			return cmd, true, fmt.Errorf("%w: /%s takes no arguments", ErrMalformedCommand, cmd.Name)
		}
		return cmd, true, nil
	}
}

// commandLine matches a command word at column 0, alone or followed by
// whitespace and arguments. The word is exact and lowercase: /Approve and
// /approved are not commands.
var commandLine = regexp.MustCompile(`^/(approve|revise|ask|cancel|status|retry|help)(?:[ \t]+(.*?))?[ \t]*$`)

// parseApprove reads /approve's arguments: at most one mode, and
// --accept-risk at most once. Errors name the rule broken, never the
// argument, so a log line carries no comment text.
func parseApprove(cmd Command, args string) (Command, error) {
	for _, arg := range strings.Fields(args) {
		switch {
		case arg == string(ApproveSequential) || arg == string(ApproveParallel):
			if cmd.Mode != ApproveDefault {
				return cmd, fmt.Errorf("%w: /approve takes one mode", ErrMalformedCommand)
			}
			cmd.Mode = ApproveMode(arg)
		case arg == "--accept-risk":
			if cmd.AcceptRisk {
				return cmd, fmt.Errorf("%w: /approve repeats --accept-risk", ErrMalformedCommand)
			}
			cmd.AcceptRisk = true
		default:
			return cmd, fmt.Errorf("%w: /approve has an unknown argument", ErrMalformedCommand)
		}
	}
	return cmd, nil
}
