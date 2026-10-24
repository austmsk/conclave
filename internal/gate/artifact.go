package gate

import (
	"context"
	"errors"

	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
)

// MaxArtifactBytes caps the raw diff Freeze accepts. A larger diff is the
// attempt's own failure (ErrArtifactTooLarge), checked before anything
// parses or scans it.
const MaxArtifactBytes = 8 << 20

// Artifact identifies one frozen diff bound to its base commit.
type Artifact struct {
	RepoID string

	// Base is the commit the diff applies to: the prepared tree's HEAD.
	Base string

	// Hash is HashArtifact(Base, diff): lowercase hex SHA-256 of the
	// envelope. It is what every GateResult records.
	Hash string

	// Key is the blob key, RepoID + "/" + Hash. blob.Get re-verifies the
	// content against it on every read.
	Key string

	// Size is the envelope's length in bytes.
	Size int64
}

// Envelope frames a diff with its base commit:
//
//	conclave-artifact v1\nbase <sha>\n\n<diff bytes>
func Envelope(base string, diff []byte) []byte {
	return nil
}

// HashArtifact returns the lowercase hex SHA-256 of Envelope(base, diff).
func HashArtifact(base string, diff []byte) string {
	return ""
}

// Freeze reads the sandbox's diff twice, requires identical bytes, and
// stores the envelope. Nothing after Freeze reads the agent's workspace.
func Freeze(ctx context.Context, sb contracts.Sandbox, blobs blob.Store, repoID, base string) (Artifact, error) {
	return Artifact{}, errors.New("not implemented")
}

// Load reads an artifact back through blob.Get, which re-hashes it, and
// returns it with its diff.
func Load(ctx context.Context, blobs blob.Store, key string) (Artifact, []byte, error) {
	return Artifact{}, nil, errors.New("not implemented")
}
