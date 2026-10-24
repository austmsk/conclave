package gate

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

// checkProtected fails a diff that touches a protected path (FR-SEC8).
// Renames and copies are checked on both paths. A path the sandbox would
// refuse (sandbox.CheckPath), and any symlink or gitlink on the new side,
// fail too: either can reach outside what the patterns describe. Milestone
// 1 has no owner approval, so a hit fails and is never only flagged.
func checkProtected(changes []FileChange, p Policy) verdict {
	v := verdict{Gate: contracts.GateProtected}
	pats, err := p.patterns()
	if err != nil {
		v.Detail = "policy patterns invalid"
		return v
	}
	var hits []string
	for _, c := range changes {
		if c.NewMode == modeSymlink || c.NewMode == modeGitlink {
			hits = append(hits, quotePath(c.NewPath)+" (symlink or gitlink)")
		}
		for _, path := range changePaths(c) {
			if why := protectedHit(path, pats.protected); why != "" {
				hits = append(hits, quotePath(path)+" ("+why+")")
			}
		}
	}
	if len(hits) > 0 {
		v.Detail = "protected paths changed: " + listSome(hits)
		return v
	}
	v.Passed = true
	v.Detail = "no protected paths changed"
	return v
}

// protectedHit returns why path is protected, or "" when it is not.
func protectedHit(path string, protected []pattern) string {
	if err := sandbox.CheckPath(path); err != nil {
		return "invalid path"
	}
	if pt, ok := matchAny(protected, path); ok {
		return "matches " + strconv.Quote(pt.raw)
	}
	return ""
}

// changePaths returns the distinct non-empty paths a change touches.
func changePaths(c FileChange) []string {
	switch {
	case c.OldPath == "":
		return []string{c.NewPath}
	case c.NewPath == "" || c.NewPath == c.OldPath:
		return []string{c.OldPath}
	}
	return []string{c.OldPath, c.NewPath}
}

// maxListed is how many items a Detail names before summarizing.
const maxListed = 5

// listSome joins the first few items and counts the rest.
func listSome(items []string) string {
	if len(items) <= maxListed {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:maxListed], ", "), len(items)-maxListed)
}
