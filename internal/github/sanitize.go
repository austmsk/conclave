package github

import (
	"regexp"
	"strings"
)

// teamMention matches an @org/team mention, which notifies every member of
// the team. It must not follow a word character, so an email address or a
// path is left alone, or a backtick, so a mention already in a code span is
// not wrapped twice.
var teamMention = regexp.MustCompile("(^|[^A-Za-z0-9_`])(@[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]+)")

// htmlComment matches an HTML comment, or an unterminated one running to
// the end of the text. GitHub renders neither, so text inside is hidden
// from the reader while it stays in the record.
var htmlComment = regexp.MustCompile(`(?s)<!--.*?(?:-->|$)`)

// sanitizeMarkdown makes text the platform posts safe to publish
// (FR-SEC28). Team mentions are wrapped in a code span, which GitHub does
// not notify for, so generated text cannot page an organisation. HTML
// comments are removed, so nothing reaches the issue thread that the owner
// cannot see. Removal repeats until none is left, because removing one
// comment can join the pieces of another. Collapsible <details> sections
// stay: the status comment uses them, and their content is a click away.
//
// Every comment and pull request body passes through it on the way out,
// whoever wrote the text.
func sanitizeMarkdown(s string) string {
	for {
		stripped := htmlComment.ReplaceAllString(s, "")
		if stripped == s {
			break
		}
		s = stripped
	}
	if !strings.Contains(s, "@") {
		return s
	}
	return teamMention.ReplaceAllString(s, "$1`$2`")
}
