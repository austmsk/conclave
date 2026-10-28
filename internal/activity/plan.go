package activity

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
)

// PlanBody is plans.body, the frozen agent input. The attempt activity
// reads it, never the issue, so what the owner approved is what runs
// (FR-SEC28). ConfigDigests records the SHA-256 of every deploy file the
// worker loaded, so each attempt's configuration is recoverable from the
// plan it ran under.
type PlanBody struct {
	Task          contracts.TaskSpec    `json:"task"`
	IssueText     string                `json:"issue_text"`
	Profile       contracts.RepoProfile `json:"profile"`
	Constraints   contracts.Constraints `json:"constraints"`
	ConfigDigests map[string]string     `json:"config_digests"`
	Issue         PlanIssue             `json:"issue"`
}

// PlanIssue identifies the issue a plan was parsed from.
type PlanIssue struct {
	Number   int    `json:"number"`
	Title    string `json:"title"`
	AuthorID int64  `json:"author_id"`
}

// TaskKey is the key of Milestone 1's single task.
const TaskKey = "T1"

// The owner-written issue sections, matched by heading, case-insensitively.
const (
	sectionTask     = "task"
	sectionCriteria = "acceptance criteria"
	sectionHints    = "hints"
)

var (
	headingRE  = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)[ \t#]*$`)
	listItemRE = regexp.MustCompile(`^[ \t]*(?:[-*+]|\d{1,9}[.)])[ \t]+(?:\[[ xX]\][ \t]+)?(.*\S)[ \t]*$`)
	fenceRE    = regexp.MustCompile("^ {0,3}(```|~~~)")
)

// section is one heading's text: everything up to the next heading of the
// same or a higher level. Deeper headings belong to it.
type section struct {
	level int
	lines []string
}

// ParseIssue derives Milestone 1's single task from an owner-written
// issue body (no model call). Task becomes the description, each item of
// Acceptance criteria one criterion (AC1, AC2, ...), and Hints is
// appended to the description under its own label. problem is non-empty
// when the issue cannot be planned, saying which section is missing.
func ParseIssue(body string) (spec contracts.TaskSpec, problem string) {
	sections := splitSections(body)
	task := strings.TrimSpace(strings.Join(sections[sectionTask].lines, "\n"))
	if task == "" {
		return contracts.TaskSpec{}, "the issue has no Task section; add one describing the change"
	}
	criteria := listItems(sections[sectionCriteria].lines)
	if len(criteria) == 0 {
		return contracts.TaskSpec{}, "the issue's Acceptance criteria section is missing or has no list items"
	}
	if hints := strings.TrimSpace(strings.Join(sections[sectionHints].lines, "\n")); hints != "" {
		task += "\n\nHints:\n" + hints
	}
	spec = contracts.TaskSpec{Key: TaskKey, Description: task}
	for i, text := range criteria {
		spec.Criteria = append(spec.Criteria, contracts.AcceptanceCriteria{ID: "AC" + strconv.Itoa(i+1), Text: text})
	}
	return spec, ""
}

// splitSections returns the recognised sections by lowercase name. A
// heading inside a fenced code block is not a heading, and a section that
// appears twice keeps its first occurrence.
func splitSections(body string) map[string]*section {
	out := map[string]*section{sectionTask: {}, sectionCriteria: {}, sectionHints: {}}
	var cur *section
	seen := map[string]bool{}
	inFence := false
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if fenceRE.MatchString(line) {
			inFence = !inFence
		}
		level, name, ok := heading(line, inFence)
		switch {
		case ok && cur != nil && level > cur.level:
			cur.lines = append(cur.lines, line)
		case ok:
			cur = nil
			if s, known := out[name]; known && !seen[name] {
				seen[name] = true
				s.level, cur = level, s
			}
		case cur != nil:
			cur.lines = append(cur.lines, line)
		}
	}
	return out
}

// heading parses an ATX heading line into its level and lowercase name,
// without a trailing colon.
func heading(line string, inFence bool) (int, string, bool) {
	if inFence {
		return 0, "", false
	}
	m := headingRE.FindStringSubmatch(line)
	if m == nil {
		return 0, "", false
	}
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(m[2]), ":"))
	return len(m[1]), name, true
}

// listItems returns the text of each list item. A non-blank line that is
// not an item continues the item before it.
func listItems(lines []string) []string {
	var items []string
	for _, line := range lines {
		if m := listItemRE.FindStringSubmatch(line); m != nil {
			items = append(items, strings.TrimSpace(m[1]))
			continue
		}
		if t := strings.TrimSpace(line); t != "" && len(items) > 0 {
			items[len(items)-1] += " " + t
		}
	}
	return items
}

// planMarker is the text by which the plan comment is found again. It is
// visible text, not an HTML comment, because the GitHub client strips
// comments from everything it posts.
func planMarker(planID string) string {
	return "conclave-plan:" + planID
}

// planComment renders the plan comment: the task, its criteria, the
// constraints, and how to approve or cancel, ending with the marker.
func planComment(planID string, body PlanBody) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Plan for #%d\n\n### Task %s\n\n%s\n\n### Acceptance criteria\n\n",
		body.Issue.Number, body.Task.Key, body.Task.Description)
	for _, c := range body.Task.Criteria {
		fmt.Fprintf(&b, "- **%s**: %s\n", c.ID, c.Text)
	}
	fmt.Fprintf(&b, "\n### Constraints\n\n- At most %d changed lines\n- %d protected paths, %d execution-surface paths\n",
		body.Constraints.MaxChangedLines, len(body.Constraints.ProtectedPaths), len(body.Constraints.ExecutionSurfacePaths))
	b.WriteString("\nReply `/approve` to start implementation against the current default branch, or `/cancel` to stop.\n\n")
	fmt.Fprintf(&b, "<sub>%s</sub>\n", planMarker(planID))
	return b.String()
}
