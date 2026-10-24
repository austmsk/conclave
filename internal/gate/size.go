package gate

import (
	"fmt"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
)

// checkDiffSize fails an empty diff and one over any of the repository's
// limits. Changed lines are added plus removed text lines; a binary file
// counts as a changed file with no lines. Limits that are not positive
// fail closed, as an unconfigured policy.
func checkDiffSize(changes []FileChange, diffBytes int, lim SizeLimits) verdict {
	v := verdict{Gate: contracts.GateDiffSize}
	if lim.MaxChangedLines <= 0 || lim.MaxChangedFiles <= 0 || lim.MaxDiffBytes <= 0 {
		v.Detail = "size limits not configured"
		return v
	}
	if len(changes) == 0 {
		v.Detail = "empty diff: no changes to gate"
		return v
	}
	lines := 0
	for _, c := range changes {
		lines += len(c.Added) + len(c.Removed)
	}
	var over []string
	if lines > lim.MaxChangedLines {
		over = append(over, fmt.Sprintf("%d changed lines exceed %d", lines, lim.MaxChangedLines))
	}
	if len(changes) > lim.MaxChangedFiles {
		over = append(over, fmt.Sprintf("%d changed files exceed %d", len(changes), lim.MaxChangedFiles))
	}
	if int64(diffBytes) > lim.MaxDiffBytes {
		over = append(over, fmt.Sprintf("%d diff bytes exceed %d", diffBytes, lim.MaxDiffBytes))
	}
	if len(over) > 0 {
		v.Detail = strings.Join(over, "; ")
		return v
	}
	v.Passed = true
	v.Detail = fmt.Sprintf("%d files, %d lines, %d bytes", len(changes), lines, diffBytes)
	return v
}
