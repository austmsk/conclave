package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
)

// MaxArtifactBytes caps the raw diff Freeze accepts. A larger diff is the
// attempt's own failure (ErrArtifactTooLarge), checked before anything
// parses or scans it.
const MaxArtifactBytes = 8 << 20

// envelopeMagic opens every artifact. A new framing gets a new version
// line, so an old hash can never be read as a new artifact.
const envelopeMagic = "conclave-artifact v1\n"

// baseRE is a full commit ID: SHA-1, or SHA-256 for repositories that use
// it. Abbreviations and ref names are refused, because the envelope must
// name exactly one commit.
var baseRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

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
//
// The diff bytes follow unchanged: nothing is normalized, so the hash
// covers exactly what was recorded.
func Envelope(base string, diff []byte) []byte {
	var b bytes.Buffer
	b.Grow(len(envelopeMagic) + len("base \n\n") + len(base) + len(diff))
	b.WriteString(envelopeMagic)
	b.WriteString("base " + base + "\n\n")
	b.Write(diff)
	return b.Bytes()
}

// HashArtifact returns the lowercase hex SHA-256 of Envelope(base, diff).
// The same diff on another base is another artifact.
func HashArtifact(base string, diff []byte) string {
	sum := sha256.Sum256(Envelope(base, diff))
	return hex.EncodeToString(sum[:])
}

// Freeze reads the sandbox's diff twice, requires identical bytes, and
// stores the envelope. The sandbox contract guarantees no process outlives
// the call that started it, so nothing can still be writing between the
// reads; the second read catches a backend that breaks that promise.
// Nothing after Freeze reads the agent's workspace.
func Freeze(ctx context.Context, sb contracts.Sandbox, blobs blob.Store, repoID, base string) (Artifact, error) {
	if !baseRE.MatchString(base) {
		return Artifact{}, fmt.Errorf("freezing artifact: base %q is not a full commit ID", base)
	}
	first, err := readDiff(ctx, sb)
	if err != nil {
		return Artifact{}, err
	}
	second, err := readDiff(ctx, sb)
	if err != nil {
		return Artifact{}, err
	}
	if !bytes.Equal(first, second) {
		return Artifact{}, fmt.Errorf("freezing artifact in sandbox %s: %w", sb.ID(), ErrUnstableDiff)
	}
	env := Envelope(base, first)
	key, err := blobs.Put(ctx, repoID, env)
	if err != nil {
		return Artifact{}, environmentf("storing artifact: %v", err)
	}
	hash := HashArtifact(base, first)
	if key != repoID+"/"+hash {
		return Artifact{}, environmentf("blob store returned key %q for artifact %s", key, hash)
	}
	return Artifact{RepoID: repoID, Base: base, Hash: hash, Key: key, Size: int64(len(env))}, nil
}

// readDiff reads the diff once and refuses one above MaxArtifactBytes.
func readDiff(ctx context.Context, sb contracts.Sandbox) ([]byte, error) {
	d, err := sb.Diff(ctx)
	if err != nil {
		return nil, environmentf("reading diff from sandbox %s: %v", sb.ID(), err)
	}
	if len(d) > MaxArtifactBytes {
		return nil, fmt.Errorf("freezing artifact: diff is %d bytes, cap %d: %w", len(d), MaxArtifactBytes, ErrArtifactTooLarge)
	}
	return d, nil
}

// Load reads an artifact back through blob.Get, which re-hashes it, and
// returns it with its diff.
func Load(ctx context.Context, blobs blob.Store, key string) (Artifact, []byte, error) {
	repoID, hash, ok := strings.Cut(key, "/")
	if !ok {
		return Artifact{}, nil, fmt.Errorf("loading artifact %q: %w", key, blob.ErrInvalidKey)
	}
	env, err := blobs.Get(ctx, key)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return Artifact{}, nil, fmt.Errorf("loading artifact %s: %w", key, err)
		}
		// Both wrapped, so a caller can tell tampering (blob.ErrCorrupt)
		// from an unreachable store.
		return Artifact{}, nil, fmt.Errorf("%w: loading artifact %s: %w", ErrEnvironment, key, err)
	}
	base, diff, err := openEnvelope(env)
	if err != nil {
		return Artifact{}, nil, fmt.Errorf("loading artifact %s: %w", key, err)
	}
	if HashArtifact(base, diff) != hash {
		return Artifact{}, nil, fmt.Errorf("loading artifact %s: %w", key, blob.ErrCorrupt)
	}
	return Artifact{RepoID: repoID, Base: base, Hash: hash, Key: key, Size: int64(len(env))}, diff, nil
}

// openEnvelope splits an envelope into its base and diff.
func openEnvelope(env []byte) (string, []byte, error) {
	rest, ok := bytes.CutPrefix(env, []byte(envelopeMagic+"base "))
	if !ok {
		return "", nil, errors.New("not a conclave artifact envelope")
	}
	base, diff, ok := bytes.Cut(rest, []byte("\n\n"))
	if !ok || !baseRE.Match(base) {
		return "", nil, errors.New("artifact envelope has no valid base line")
	}
	return string(base), diff, nil
}
