package gate

import "errors"

// ErrMalformedDiff reports diff bytes the parser refuses. The diff comes
// from the agent's sandbox, so anything unexpected fails closed.
var ErrMalformedDiff = errors.New("malformed diff")

// ChangeKind is what a diff did to one file.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeDeleted  ChangeKind = "deleted"
	ChangeModified ChangeKind = "modified"
	ChangeRenamed  ChangeKind = "renamed"
	ChangeCopied   ChangeKind = "copied"
)

// FileChange is one file's entry in a git diff --binary.
type FileChange struct {
	Kind ChangeKind

	// OldPath and NewPath are unquoted, slash-separated, without the a/
	// and b/ prefixes. OldPath is empty for an added file and NewPath for
	// a deleted one.
	OldPath string
	NewPath string

	// OldMode and NewMode are git modes such as "100644", "100755",
	// "120000" (symlink) or "160000" (gitlink); empty when absent.
	OldMode string
	NewMode string

	// Binary is set for a GIT binary patch or "Binary files differ".
	Binary bool

	// Added and Removed are the text lines, without their +/- prefix.
	Added   []string
	Removed []string
}

// ParseDiff parses git diff --binary output strictly and within bounds.
func ParseDiff(diff []byte) ([]FileChange, error) {
	return nil, errors.New("not implemented")
}
