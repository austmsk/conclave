package gate

import (
	"strconv"

	"github.com/austmsk/conclave/internal/contracts"
)

// checkExecSurface flags, and never fails, a diff that changes the
// execution surface (FR-SEC25): a path matching the surface patterns on
// either side of the change, or a file that becomes executable. A flag
// moves gating to clean_room and is surfaced on the pull request.
func checkExecSurface(changes []FileChange, p Policy) verdict {
	v := verdict{Gate: contracts.GateExecSurface, Passed: true}
	pats, err := p.patterns()
	if err != nil {
		// Fail closed: an unreadable policy cannot show the diff is clear.
		v.Flagged = true
		v.Detail = "policy patterns invalid"
		return v
	}
	var hits []string
	for _, c := range changes {
		if becomesExecutable(c) {
			hits = append(hits, quotePath(c.NewPath)+" (made executable)")
		}
		for _, path := range changePaths(c) {
			if pt, ok := matchAny(pats.surface, path); ok {
				hits = append(hits, quotePath(path)+" (matches "+strconv.Quote(pt.raw)+")")
			}
		}
	}
	if len(hits) > 0 {
		v.Flagged = true
		v.Detail = "execution surface changed: " + listSome(hits)
		return v
	}
	v.Detail = "no execution-surface changes"
	return v
}

// becomesExecutable reports a change whose new side is an executable file
// that was not one before: a new or copied file with mode 100755, or a
// mode change to it.
func becomesExecutable(c FileChange) bool {
	if c.NewMode != modeExecutable {
		return false
	}
	switch c.Kind {
	case ChangeAdded, ChangeCopied:
		return true
	}
	return c.OldMode != modeExecutable
}
