package gate

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/austmsk/conclave/internal/sandbox"
)

// match.go matches repository paths against the patterns in defaults.go
// and gates.yaml. The owner may check the branch out on a case-insensitive
// or HFS+ filesystem, so both sides are compared folded
// (sandbox.FoldName): ".Claude/" and ".cl\u200caude/" are ".claude/".
//
// Pattern syntax, deliberately smaller than gitignore's:
//   - A pattern with no inner "/" matches a name at any depth: "Makefile"
//     matches "Makefile", "sub/Makefile" and anything under a directory
//     named Makefile.
//   - A pattern with an inner "/" is anchored at the repository root:
//     "gradle/wrapper" matches "gradle/wrapper/x.jar" but not
//     "sub/gradle/wrapper/x.jar".
//   - A trailing "/" means a directory: ".husky/" matches "a/.husky/pre-commit"
//     but not a file named ".husky".
//   - Within a component, path.Match syntax applies ("*", "?", "[...]").
//     "**", a leading "/", backslashes and empty, "." or ".." components are
//     refused when the pattern is compiled.
//
// A pattern that matches a path also matches every path under it, so a
// pattern naming a file covers a directory of the same name too.

// pattern is one compiled path pattern.
type pattern struct {
	raw      string
	parts    []string // folded components
	anchored bool
	dir      bool
}

var errBadPattern = errors.New("invalid path pattern")

// compilePattern parses and folds one pattern.
func compilePattern(raw string) (pattern, error) {
	s, dir := strings.CutSuffix(raw, "/")
	switch {
	case s == "":
		return pattern{}, fmt.Errorf("%w: %q: empty", errBadPattern, raw)
	case strings.HasPrefix(s, "/"):
		return pattern{}, fmt.Errorf("%w: %q: starts with /; a pattern with an inner / is already anchored", errBadPattern, raw)
	case strings.Contains(s, "**"):
		return pattern{}, fmt.Errorf("%w: %q: ** is not supported", errBadPattern, raw)
	case strings.ContainsAny(s, "\\\x00"):
		return pattern{}, fmt.Errorf("%w: %q: contains a backslash or NUL", errBadPattern, raw)
	}
	comps := strings.Split(s, "/")
	p := pattern{raw: raw, anchored: len(comps) > 1, dir: dir}
	for _, c := range comps {
		folded := sandbox.FoldName(c)
		if c == "." || c == ".." || folded == "" {
			return pattern{}, fmt.Errorf("%w: %q: empty or dot component", errBadPattern, raw)
		}
		if _, err := path.Match(folded, ""); err != nil {
			return pattern{}, fmt.Errorf("%w: %q: %w", errBadPattern, raw, err)
		}
		p.parts = append(p.parts, folded)
	}
	return p, nil
}

// compilePatterns compiles each pattern in order.
func compilePatterns(raws ...[]string) ([]pattern, error) {
	var out []pattern
	for _, list := range raws {
		for _, raw := range list {
			p, err := compilePattern(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// match reports whether the slash-separated repository path p, or a
// directory containing it, is what the pattern names.
func (pt pattern) match(p string) bool {
	comps := foldPath(p)
	if pt.anchored {
		return pt.matchAt(comps, 0)
	}
	for i := range comps {
		if pt.matchAt(comps, i) {
			return true
		}
	}
	return false
}

// matchAt reports whether the pattern's components match comps from i.
// A directory pattern must leave at least one component below it.
func (pt pattern) matchAt(comps []string, i int) bool {
	end := i + len(pt.parts)
	if end > len(comps) || (pt.dir && end == len(comps)) {
		return false
	}
	for j, part := range pt.parts {
		if ok, _ := path.Match(part, comps[i+j]); !ok {
			return false
		}
	}
	return true
}

func foldPath(p string) []string {
	comps := strings.Split(p, "/")
	for i, c := range comps {
		comps[i] = sandbox.FoldName(c)
	}
	return comps
}

// matchAny returns the first pattern in ps that matches p.
func matchAny(ps []pattern, p string) (pattern, bool) {
	for _, pt := range ps {
		if pt.match(p) {
			return pt, true
		}
	}
	return pattern{}, false
}
