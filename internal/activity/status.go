package activity

import (
	"fmt"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
)

// The words of the status comment and the command replies. Every command
// is mentioned in backticks, and no line starts with a slash, so nothing
// the platform posts reads as a command (NFR-U2a). Text that comes from an
// error is flattened onto one line inside a code span for the same
// reason.

// statusMarker is the text by which the status comment is found again.
// Like planMarker it is visible text, because the GitHub client strips
// HTML comments from everything it posts.
func statusMarker(featureRequestID string) string {
	return "conclave-status:" + featureRequestID
}

// replyMarker is the text by which a command's reply is found again: one
// reply per command comment.
func replyMarker(commentID int64) string {
	return fmt.Sprintf("conclave-reply:%d", commentID)
}

// commandHelp is what each command does, for /help.
var commandHelp = map[github.CommandName]string{
	github.CommandApprove: "approve the plan and start implementation",
	github.CommandRevise:  "revise the plan (not available in Milestone 1)",
	github.CommandAsk:     "ask about the plan or the run (not available in Milestone 1)",
	github.CommandCancel:  "stop the run",
	github.CommandStatus:  "show the run's status",
	github.CommandRetry:   "continue in the phase the run left, after checking the caps",
	github.CommandHelp:    "list the commands available now",
}

// header is the first line of every platform comment: the feature, and
// the plan version Milestone 1 runs.
func header(issue int) string {
	return fmt.Sprintf("**Conclave** · #%d · plan v%d\n\n", issue, PlanVersion)
}

// statusComment renders the status comment: the status, the reason and
// the cap when there is one, the phase it left, and the commands
// available, ending with the marker.
func statusComment(issue int, v RunView) string {
	var b strings.Builder
	b.WriteString(header(issue))
	writeView(&b, v)
	writeCap(&b, v.Cap)
	b.WriteString("\n" + commandsLine(v.Commands) + "\n\n")
	fmt.Fprintf(&b, "<sub>%s</sub>\n", statusMarker(v.FeatureRequestID))
	return b.String()
}

// writeView writes the status, and in needs_attention the reason and the
// phase left.
func writeView(b *strings.Builder, v RunView) {
	fmt.Fprintf(b, "**Status:** `%s`\n", v.Status)
	if v.Status != contracts.FeatureNeedsAttention {
		return
	}
	if v.Reason != "" {
		fmt.Fprintf(b, "\n**Reason:** %s\n", describeReason(v.Reason))
	}
	if v.PhaseLeft != "" {
		fmt.Fprintf(b, "\n**Left from:** `%s`; `/retry` continues there.\n", v.PhaseLeft)
	}
}

// writeCap writes a refused /retry's cap figures.
func writeCap(b *strings.Builder, c *CapCheck) {
	if c == nil || c.Exceeded == "" {
		return
	}
	fmt.Fprintf(b, "\n**`/retry` refused:** %s\n", describeCapCheck(*c))
}

// describeReason turns a feature request reason into a sentence: the kind,
// and the cap or the error detail.
func describeReason(reason string) string {
	kind, detail, _ := strings.Cut(reason, ":")
	switch kind {
	case ReasonBudgetCap:
		return "a platform cap was hit: the " + capName(llm.BudgetKind(detail)) + "."
	case ReasonAttemptFailed:
		return "the attempt finished, but it did not pass the gates."
	case ReasonError:
		return "something went wrong: " + codeSpan(detail)
	default:
		return codeSpan(reason)
	}
}

// capName names a cap and the configuration key that sets it.
func capName(kind llm.BudgetKind) string {
	switch kind {
	case llm.BudgetFeatureRequest:
		return "per-feature-request token cap (`budgets.feature_request_tokens` in `deploy/router.yaml`)"
	case llm.BudgetMonth:
		return "monthly spending cap (`budgets.monthly_usd` in `deploy/router.yaml`)"
	case "":
		return "cap, which was not recorded"
	default:
		return codeSpan(string(kind)) + " cap"
	}
}

// describeCapCheck says which cap is still exceeded, by how much, and how
// to raise it.
func describeCapCheck(c CapCheck) string {
	var spent string
	switch c.Exceeded {
	case llm.BudgetFeatureRequest:
		spent = fmt.Sprintf("%d of %d tokens", c.FeatureRequestTokens, c.FeatureRequestCap)
	case llm.BudgetMonth:
		spent = fmt.Sprintf("$%.2f of $%.2f this month", c.MonthUSD, c.MonthCap)
	}
	return fmt.Sprintf("the feature request is still at or over the %s, with %s spent. "+
		"Raise the cap, restart the worker, and reply `/retry` again.", capName(c.Exceeded), spent)
}

// commandsLine lists the commands available, each in backticks.
func commandsLine(cmds []github.CommandName) string {
	if len(cmds) == 0 {
		return "No commands are available in this status."
	}
	return "Commands available: " + commandList(cmds) + "."
}

func commandList(cmds []github.CommandName) string {
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = "`/" + string(c) + "`"
	}
	return strings.Join(names, ", ")
}

// codeSpan puts text from an error on one line inside a code span, so it
// can neither start a line nor close the span early.
func codeSpan(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "`", "'")
	if s == "" {
		return "(no detail)"
	}
	return "`" + s + "`"
}

// replyBody renders one command's reply, ending with its marker.
func replyBody(in ReplyInput) string {
	var b strings.Builder
	b.WriteString(header(in.Ref.Issue))
	b.WriteString(replyText(in))
	fmt.Fprintf(&b, "\n\n<sub>%s</sub>\n", replyMarker(in.Ref.CommentID))
	return b.String()
}

// replyText is a reply's message. Every rejection says that nothing
// changed.
func replyText(in ReplyInput) string {
	cmd := "`/" + string(in.Ref.Command) + "`"
	v := in.View
	switch in.Kind {
	case ReplyNotValid:
		return fmt.Sprintf("%s is not valid while this run is `%s`; nothing changed. %s", cmd, v.Status, commandsLine(v.Commands))
	case ReplyNotAvailable:
		return fmt.Sprintf("%s is not available in Milestone 1; nothing changed. %s", cmd, commandsLine(v.Commands))
	case ReplyStatus:
		var b strings.Builder
		writeView(&b, v)
		return b.String() + "\n" + commandsLine(v.Commands)
	case ReplyHelp:
		return helpText(v)
	case ReplyOverCap:
		return fmt.Sprintf("%s refused: %s The run stays `%s`.", cmd, describeCapCheck(derefCap(v.Cap)), v.Status)
	case ReplyResumeFailed:
		return fmt.Sprintf("%s could not resume the run: %s The run stays `%s`; reply `/retry` to try again.", cmd, codeSpan(in.Detail), v.Status)
	case ReplyAlreadyPending:
		return fmt.Sprintf("An earlier %s is already being acted on; this one changed nothing.", cmd)
	case ReplyCancelled:
		return fmt.Sprintf("%s was not applied: this run is being cancelled, and the cancel takes precedence. "+
			"If the run does not reach `cancelled`, reply `/cancel` again.", cmd)
	case ReplyClosedRun:
		if v.Status == contracts.FeatureDelivered && in.Ref.Command == github.CommandRevise {
			return cmd + " on a delivered run is not available in Milestone 1; nothing changed."
		}
		return fmt.Sprintf("This run is `%s` and accepts no commands; %s changed nothing.", v.Status, cmd)
	case ReplyNoRun:
		return fmt.Sprintf("There is no Conclave run on this issue; %s changed nothing.", cmd)
	case ReplyNoWorkflow:
		return fmt.Sprintf("This run is `%s`, but its workflow is not running, so %s changed nothing.", v.Status, cmd)
	default:
		return fmt.Sprintf("%s changed nothing.", cmd)
	}
}

func helpText(v RunView) string {
	if len(v.Commands) == 0 {
		return fmt.Sprintf("No commands are available while this run is `%s`.", v.Status)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Commands available while this run is `%s`:\n\n", v.Status)
	for _, c := range v.Commands {
		fmt.Fprintf(&b, "- `/%s`: %s\n", c, commandHelp[c])
	}
	return strings.TrimRight(b.String(), "\n")
}

func derefCap(c *CapCheck) CapCheck {
	if c == nil {
		return CapCheck{}
	}
	return *c
}
