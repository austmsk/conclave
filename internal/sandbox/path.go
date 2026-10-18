package sandbox

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxPathLength = 4096

var percentEscape = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// CheckPath validates a caller-supplied path relative to the workspace.
// Every rule is a rejection; nothing is rewritten, so a path that passes is
// exactly what the caller asked for. Every backend and the agent's tools
// apply the same rules, so a path one layer accepts no other refuses.
//
// The sandbox's filesystem is case-sensitive Linux, but the branch it
// produces is checked out by the owner, possibly on macOS or Windows. A
// name those filesystems resolve to .git is refused here too: see
// IsGitName.
func CheckPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: empty", ErrInvalidPath)
	case len(p) > maxPathLength:
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidPath, maxPathLength)
	case !utf8.ValidString(p):
		return fmt.Errorf("%w: not valid UTF-8: %q", ErrInvalidPath, p)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%w: absolute: %q", ErrInvalidPath, p)
	case strings.HasSuffix(p, "/"):
		return fmt.Errorf("%w: trailing slash: %q", ErrInvalidPath, p)
	case strings.ContainsAny(p, "\\\x00"):
		return fmt.Errorf("%w: contains a backslash or NUL: %q", ErrInvalidPath, p)
	case percentEscape.MatchString(p):
		return fmt.Errorf("%w: contains a percent escape: %q", ErrInvalidPath, p)
	}
	if what := unsafeRunes(p); what != "" {
		return fmt.Errorf("%w: contains %s: %q", ErrInvalidPath, what, p)
	}
	for _, component := range strings.Split(p, "/") {
		if err := checkComponent(component); err != nil {
			return fmt.Errorf("%w: %q", err, p)
		}
	}
	return nil
}

func checkComponent(c string) error {
	switch {
	case c == "":
		return fmt.Errorf("%w: empty component", ErrInvalidPath)
	case c == "." || c == "..":
		return fmt.Errorf("%w: dot component", ErrInvalidPath)
	case strings.Trim(c, ".") == "":
		// "...." and friends are what "....//" collapses to once a
		// naive sanitizer removes "../"; reject the shape outright.
		return fmt.Errorf("%w: dots-only component", ErrInvalidPath)
	case IsGitName(c):
		return fmt.Errorf("%w: reaches into .git", ErrInvalidPath)
	}
	return nil
}

// unsafeRunes names the first kind of character p must not contain, or
// returns "" when it has none. Control characters can hide a path's real
// shape from whoever reads a log or a diff; bidirectional controls can
// make it display as a different path altogether.
func unsafeRunes(p string) string {
	for _, r := range p {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return "control characters"
		case isBidiControl(r):
			return "bidirectional controls"
		}
	}
	return ""
}

// isBidiControl reports the explicit directional marks, embeddings,
// overrides and isolates.
func isBidiControl(r rune) bool {
	switch {
	case r == 0x061c, r == 0x200e, r == 0x200f:
		return true
	case r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// isHFSIgnorable reports the code points HFS+ drops when it compares
// names, so ".g\u200cit" opens .git on macOS. The list is git's own (see
// is_hfs_dotgit in git's utf8.c).
func isHFSIgnorable(r rune) bool {
	switch {
	case r >= 0x200c && r <= 0x200f:
		return true
	case r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x206a && r <= 0x206f:
		return true
	}
	return r == 0xfeff
}

// FoldName returns the form of one path component that case-insensitive,
// HFS+ and NTFS filesystems compare: HFS+-ignorable code points removed,
// lower-cased, cut at an NTFS alternate-stream colon (".git::$INDEX_ALLOCATION"
// is .git), and without the trailing dots and spaces NTFS discards. Two
// components with the same fold may name the same file on the owner's
// checkout, so anything that refuses a name refuses every name that folds
// to it.
func FoldName(c string) string {
	c = strings.Map(func(r rune) rune {
		if isHFSIgnorable(r) {
			return -1
		}
		return r
	}, c)
	c = strings.ToLower(c)
	if i := strings.IndexByte(c, ':'); i >= 0 {
		c = c[:i]
	}
	return strings.TrimRight(c, ". ")
}

// IsGitName reports whether one path component may name the repository's
// .git directory on some filesystem the branch can be checked out on: any
// spelling that folds to ".git" (see FoldName), or "git~1", the NTFS short
// name Windows gives .git. ".gitignore" and ".github" are ordinary names.
func IsGitName(c string) bool {
	f := FoldName(c)
	return f == ".git" || f == "git~1"
}
