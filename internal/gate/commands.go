package gate

import "time"

// Commands are one repository's build, lint and test commands, as argv
// lists run in the workspace root (FR-I4). Test must write a JUnit XML
// report under JUnitDir.
type Commands struct {
	Build []string
	Lint  []string
	Test  []string

	// Timeout bounds each command.
	Timeout time.Duration
}

// JUnitDir is where the test command writes its JUnit XML report. It is
// under /tmp, so nothing lands in the diff, and it is deleted before the
// tests run.
const JUnitDir = "/tmp/conclave-junit"
