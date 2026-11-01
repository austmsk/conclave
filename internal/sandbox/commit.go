package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrCommitSpec reports a CommitSpec the worker refuses to act on.
	ErrCommitSpec = errors.New("invalid commit spec")
	// ErrCommitApply means git refused to apply the diff to the parent's
	// tree. git's output is not kept: it can quote names from the diff.
	ErrCommitApply = errors.New("diff does not apply to the parent commit")
)

// fullCommitID is a full commit ID, SHA-1 or SHA-256.
var fullCommitID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// CommitSpec is one commit to build in a prepared tree on the worker.
type CommitSpec struct {
	// Dir is a prepared tree (VerifyTree must pass) whose object
	// database holds Parent.
	Dir string
	// Parent is the full commit ID the commit is made on.
	Parent string
	// Diff is applied to Parent's tree; empty means the parent's tree.
	Diff []byte
	// Message is the whole commit message, used as given.
	Message string
	// AuthorName and AuthorEmail are both author and committer.
	AuthorName  string
	AuthorEmail string
	// When is both the author and the committer date.
	When time.Time
}

// CommitDiff builds one commit of spec.Diff on spec.Parent and returns
// its ID. It touches neither the work tree, the real index, nor any ref:
// the diff is applied to a temporary index read from the parent, outside
// the tree, so nothing in the diff is ever checked out on the worker and
// no filter or hook can run. Every input of the commit is fixed by spec,
// so the same spec gives the same commit ID in any clone holding Parent:
// a retried push of it is a no-op. The new objects stay unreferenced in
// the tree's object database for a push by ID, and the tree is prepared
// again, so it still passes VerifyTree.
//
// git runs with an environment built from scratch: no system or global
// configuration, no home, no prompts.
func CommitDiff(ctx context.Context, spec CommitSpec) (string, error) {
	if err := validateCommitSpec(spec); err != nil {
		return "", err
	}
	scratch, err := os.MkdirTemp("", "conclave-commit-")
	if err != nil {
		return "", fmt.Errorf("creating commit scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	g := commitGit{dir: spec.Dir, vars: commitVars(spec, filepath.Join(scratch, "index"))}
	if _, err := g.run(ctx, "read-tree", spec.Parent); err != nil {
		return "", fmt.Errorf("reading the tree of %s: %w", spec.Parent, err)
	}
	if len(spec.Diff) > 0 {
		if err := g.apply(ctx, scratch, spec.Diff); err != nil {
			return "", err
		}
	}
	tree, err := g.run(ctx, "write-tree")
	if err != nil {
		return "", fmt.Errorf("writing the tree: %w", err)
	}
	msg := filepath.Join(scratch, "message")
	if err := os.WriteFile(msg, []byte(spec.Message), 0o600); err != nil {
		return "", fmt.Errorf("writing the commit message: %w", err)
	}
	commit, err := g.run(ctx, "commit-tree", tree, "-p", spec.Parent, "-F", msg)
	if err != nil {
		return "", fmt.Errorf("committing the tree: %w", err)
	}
	if !fullCommitID.MatchString(commit) {
		return "", fmt.Errorf("git commit-tree printed %q, not a commit ID", commit)
	}
	if err := PrepareTree(spec.Dir); err != nil {
		return "", fmt.Errorf("preparing the tree after committing: %w", err)
	}
	return commit, nil
}

func validateCommitSpec(spec CommitSpec) error {
	switch {
	case !filepath.IsAbs(spec.Dir):
		return fmt.Errorf("%w: tree %q is not an absolute path", ErrCommitSpec, spec.Dir)
	case !fullCommitID.MatchString(spec.Parent):
		return fmt.Errorf("%w: parent %q is not a full commit ID", ErrCommitSpec, spec.Parent)
	case strings.TrimSpace(spec.Message) == "":
		return fmt.Errorf("%w: empty message", ErrCommitSpec)
	case spec.When.IsZero():
		return fmt.Errorf("%w: no commit date", ErrCommitSpec)
	}
	for _, f := range []struct{ name, v string }{{"author name", spec.AuthorName}, {"author email", spec.AuthorEmail}} {
		if strings.TrimSpace(f.v) == "" || strings.ContainsAny(f.v, "<>\n\r\x00") {
			return fmt.Errorf("%w: %s %q is empty or holds <, >, a newline or NUL", ErrCommitSpec, f.name, f.v)
		}
	}
	if err := VerifyTree(spec.Dir); err != nil {
		return fmt.Errorf("%w: %w", ErrCommitSpec, err)
	}
	return nil
}

// commitVars is git's whole environment for one commit: nothing of the
// worker's but PATH, the temporary index, and the fixed identity and
// dates.
func commitVars(spec CommitSpec, index string) []string {
	date := "@" + strconv.FormatInt(spec.When.UTC().Unix(), 10) + " +0000"
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"LANG=C",
		"LC_ALL=C",
		"GIT_INDEX_FILE=" + index,
		"GIT_AUTHOR_NAME=" + spec.AuthorName,
		"GIT_AUTHOR_EMAIL=" + spec.AuthorEmail,
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + spec.AuthorName,
		"GIT_COMMITTER_EMAIL=" + spec.AuthorEmail,
		"GIT_COMMITTER_DATE=" + date,
	}
}

// commitGit runs git in one tree with one set of variables.
type commitGit struct {
	dir  string
	vars []string
}

// run runs git and returns its trimmed standard output. A failure's
// error names the subcommand and exit status only.
func (g commitGit) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	cmd.Env = g.vars
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}

// apply applies diff to the temporary index. A refusal is ErrCommitApply.
func (g commitGit) apply(ctx context.Context, scratch string, diff []byte) error {
	path := filepath.Join(scratch, "diff")
	if err := os.WriteFile(path, diff, 0o600); err != nil {
		return fmt.Errorf("writing the diff: %w", err)
	}
	_, err := g.run(ctx, "apply", "--cached", "--binary", path)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return err
	case errors.As(err, &exit):
		return ErrCommitApply
	}
	return fmt.Errorf("applying the diff: %w", err)
}
