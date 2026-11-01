package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// ErrAlreadyRecorded means a write-once pull request column already holds
// a different value. The caller asked to record something other than
// what an earlier run recorded, which a retry never does.
var ErrAlreadyRecorded = errors.New("pull request already records a different value")

// commitRE is a full commit ID, SHA-1 or SHA-256, as the CHECKs in
// migration 00009 require.
var commitRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// PullRequestRecord is one pull_requests row: a delivery of one attempt's
// artifact (migration 00009). The fields after CommitMessage are empty
// until the step that fills them has run, and each is written once.
type PullRequestRecord struct {
	ID                   string
	FeatureRequestID     string
	TaskID               string
	AttemptID            string
	IntegrationBranch    string
	PRBranch             string
	OriginalBaseSHA      string
	OriginalArtifactHash string

	Title         string
	Summary       string
	Risks         string
	CommitMessage string

	LandedCommit        string
	BaseSHA             string
	RebasedArtifactHash string
	HeadCommit          string
	Number              int
	NodeID              string
	URL                 string
	Ready               bool
}

const pullRequestColumns = `id::text, feature_request_id::text, task_id::text, attempt_id::text,
	integration_branch, pr_branch, original_base_sha, original_artifact_hash,
	title, summary, risks, commit_message,
	coalesce(landed_commit, ''), coalesce(base_sha, ''), coalesce(rebased_artifact_hash, ''),
	coalesce(head_commit, ''), coalesce(number, 0), coalesce(node_id, ''), coalesce(url, ''),
	ready_at IS NOT NULL`

func scanPullRequest(row pgx.Row) (PullRequestRecord, error) {
	var p PullRequestRecord
	err := row.Scan(&p.ID, &p.FeatureRequestID, &p.TaskID, &p.AttemptID,
		&p.IntegrationBranch, &p.PRBranch, &p.OriginalBaseSHA, &p.OriginalArtifactHash,
		&p.Title, &p.Summary, &p.Risks, &p.CommitMessage,
		&p.LandedCommit, &p.BaseSHA, &p.RebasedArtifactHash, &p.HeadCommit, &p.Number, &p.NodeID, &p.URL, &p.Ready)
	return p, err
}

// CreatePullRequest inserts the delivery record of one attempt, written
// ahead of any outside action, and returns the row as stored. It is
// idempotent on the attempt (RL-2): when a record already exists it is
// returned unchanged with created false, so the pr_writer output and
// commit message an earlier run stored are the ones every later step
// uses. Only the identity, branches, original artifact and pr_writer
// output are taken from rec; the step columns start empty.
func CreatePullRequest(ctx context.Context, db DBTX, rec PullRequestRecord) (PullRequestRecord, bool, error) {
	if err := checkNewPullRequest(rec); err != nil {
		return PullRequestRecord{}, false, fmt.Errorf("recording pull request of attempt %s: %w", rec.AttemptID, err)
	}
	const insert = `INSERT INTO pull_requests
		(feature_request_id, task_id, attempt_id, integration_branch, pr_branch,
		 original_base_sha, original_artifact_hash, title, summary, risks, commit_message)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (attempt_id) DO NOTHING`
	tag, err := db.Exec(ctx, insert, rec.FeatureRequestID, rec.TaskID, rec.AttemptID, rec.IntegrationBranch, rec.PRBranch,
		rec.OriginalBaseSHA, rec.OriginalArtifactHash, rec.Title, rec.Summary, rec.Risks, rec.CommitMessage)
	if err != nil {
		return PullRequestRecord{}, false, fmt.Errorf("recording pull request of attempt %s: %w", rec.AttemptID, err)
	}
	stored, err := PullRequestForAttempt(ctx, db, rec.AttemptID)
	if err != nil {
		return PullRequestRecord{}, false, err
	}
	return stored, tag.RowsAffected() == 1, nil
}

func checkNewPullRequest(rec PullRequestRecord) error {
	for _, id := range []struct{ field, v string }{
		{"feature request ID", rec.FeatureRequestID}, {"task ID", rec.TaskID}, {"attempt ID", rec.AttemptID},
	} {
		if err := checkUUID(id.field, id.v); err != nil {
			return err
		}
	}
	switch {
	case !commitRE.MatchString(rec.OriginalBaseSHA):
		return fmt.Errorf("original base %q is not a full commit ID", rec.OriginalBaseSHA)
	case !artifactHashRE.MatchString(rec.OriginalArtifactHash):
		return fmt.Errorf("original artifact hash %q is not lowercase hex SHA-256", rec.OriginalArtifactHash)
	case strings.TrimSpace(rec.Title) == "" || rec.CommitMessage == "":
		return errors.New("no title or no commit message")
	}
	for _, s := range []string{rec.IntegrationBranch, rec.PRBranch, rec.Title, rec.Summary, rec.Risks, rec.CommitMessage} {
		if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			return errors.New("text that Postgres cannot store: invalid UTF-8 or NUL")
		}
	}
	return nil
}

// PullRequestForAttempt returns the delivery record of one attempt, or
// ErrNotFound.
func PullRequestForAttempt(ctx context.Context, db DBTX, attemptID string) (PullRequestRecord, error) {
	if err := checkUUID("attempt ID", attemptID); err != nil {
		return PullRequestRecord{}, fmt.Errorf("reading pull request: %w", err)
	}
	p, err := scanPullRequest(db.QueryRow(ctx, `SELECT `+pullRequestColumns+` FROM pull_requests WHERE attempt_id = $1`, attemptID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PullRequestRecord{}, fmt.Errorf("pull request of attempt %s: %w", attemptID, ErrNotFound)
	}
	if err != nil {
		return PullRequestRecord{}, fmt.Errorf("reading pull request of attempt %s: %w", attemptID, err)
	}
	return p, nil
}

// Write-once columns of pull_requests that hold a commit or a hash. The
// names are constants, never input.
const (
	ColLandedCommit        = "landed_commit"
	ColBaseSHA             = "base_sha"
	ColRebasedArtifactHash = "rebased_artifact_hash"
	ColHeadCommit          = "head_commit"
)

// SetPullRequestValue records one write-once commit or hash column of a
// pull request: ColLandedCommit, ColBaseSHA, ColHeadCommit (full commit
// IDs) or ColRebasedArtifactHash (lowercase hex SHA-256). The same value
// again is a no-op, so a retried step succeeds; a different value is
// ErrAlreadyRecorded, refused here before migration 00009's trigger would
// abort the transaction.
func SetPullRequestValue(ctx context.Context, db DBTX, id, column, value string) error {
	if err := checkUUID("pull request ID", id); err != nil {
		return fmt.Errorf("recording %s: %w", column, err)
	}
	re := commitRE
	switch column {
	case ColLandedCommit, ColBaseSHA, ColHeadCommit:
	case ColRebasedArtifactHash:
		re = artifactHashRE
	default:
		return fmt.Errorf("recording pull request %s: %q is not a write-once value column", id, column)
	}
	if !re.MatchString(value) {
		return fmt.Errorf("recording %s of pull request %s: %q is malformed", column, id, value)
	}
	set := `UPDATE pull_requests SET ` + column + ` = $2 WHERE id = $1 AND (` + column + ` IS NULL OR ` + column + ` = $2)`
	return setOnce(ctx, db, id, column, set, value)
}

// SetPullRequestOpened records the opened pull request: its number, node
// ID and URL, once, after its head commit.
func SetPullRequestOpened(ctx context.Context, db DBTX, id string, number int, nodeID, url string) error {
	if err := checkUUID("pull request ID", id); err != nil {
		return fmt.Errorf("recording opened pull request: %w", err)
	}
	if number <= 0 || nodeID == "" || url == "" {
		return fmt.Errorf("recording opened pull request %s: number, node ID and URL are all required", id)
	}
	const set = `UPDATE pull_requests SET number = $2, node_id = $3, url = $4
		WHERE id = $1 AND head_commit IS NOT NULL
		  AND (number IS NULL OR (number = $2 AND node_id = $3 AND url = $4))`
	return setOnce(ctx, db, id, "number", set, number, nodeID, url)
}

// MarkPullRequestReady records that the pull request was marked ready
// for review. It is set once; a second call is a no-op.
func MarkPullRequestReady(ctx context.Context, db DBTX, id string) error {
	if err := checkUUID("pull request ID", id); err != nil {
		return fmt.Errorf("recording ready pull request: %w", err)
	}
	const set = `UPDATE pull_requests SET ready_at = now() WHERE id = $1 AND number IS NOT NULL AND ready_at IS NULL`
	tag, err := db.Exec(ctx, set, id)
	if err != nil {
		return fmt.Errorf("recording pull request %s ready: %w", id, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var ready, opened bool
	err = db.QueryRow(ctx, `SELECT ready_at IS NOT NULL, number IS NOT NULL FROM pull_requests WHERE id = $1`, id).Scan(&ready, &opened)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("pull request %s: %w", id, ErrNotFound)
	case err != nil:
		return fmt.Errorf("reading pull request %s: %w", id, err)
	case !opened:
		return fmt.Errorf("pull request %s is not opened yet: %w", id, ErrAlreadyRecorded)
	}
	return nil
}

// setOnce runs a guarded write-once UPDATE. No row changed means the
// record is missing or already holds another value.
func setOnce(ctx context.Context, db DBTX, id, column, set string, args ...any) error {
	tag, err := db.Exec(ctx, set, append([]any{id}, args...)...)
	if err != nil {
		return fmt.Errorf("recording %s of pull request %s: %w", column, id, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var found bool
	if err := db.QueryRow(ctx, `SELECT true FROM pull_requests WHERE id = $1`, id).Scan(&found); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("pull request %s: %w", id, ErrNotFound)
		}
		return fmt.Errorf("reading pull request %s: %w", id, err)
	}
	return fmt.Errorf("recording %s of pull request %s: %w", column, id, ErrAlreadyRecorded)
}

// CICheck is one required check as observed.
type CICheck struct {
	Name       string `json:"name"`
	ID         int64  `json:"id"`
	Conclusion string `json:"conclusion"`
}

// CIResult is one completed CI observation of a pull request's head
// commit, a row of the append-only pr_ci_results.
type CIResult struct {
	PullRequestID string
	HeadCommit    string
	Passed        bool
	Checks        []CICheck
	ObservedAt    time.Time
}

// Observation is the result's dedupe key: the SHA-256 of the head commit
// and each check's name, ID and conclusion, in order. The same runs read
// twice give the same key; a re-run of CI gives new IDs and a new key.
func (r CIResult) Observation() string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n", r.HeadCommit)
	for _, c := range r.Checks {
		_, _ = fmt.Fprintf(h, "%q %d %q\n", c.Name, c.ID, c.Conclusion)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RecordCIResult appends one completed observation. The same observation
// again is absorbed by the table's unique key, so a retried read records
// one row (RL-2). It returns whether a row was written.
func RecordCIResult(ctx context.Context, db DBTX, r CIResult) (bool, error) {
	if err := checkUUID("pull request ID", r.PullRequestID); err != nil {
		return false, fmt.Errorf("recording CI result: %w", err)
	}
	if !commitRE.MatchString(r.HeadCommit) || len(r.Checks) == 0 {
		return false, fmt.Errorf("recording CI result of pull request %s: needs a full head commit and at least one check", r.PullRequestID)
	}
	checks, err := json.Marshal(r.Checks)
	if err != nil {
		return false, fmt.Errorf("encoding CI checks of pull request %s: %w", r.PullRequestID, err)
	}
	if !utf8.Valid(checks) || strings.Contains(string(checks), `\u0000`) {
		return false, fmt.Errorf("recording CI result of pull request %s: check names Postgres cannot store", r.PullRequestID)
	}
	const insert = `INSERT INTO pr_ci_results (pull_request_id, head_commit, observation, passed, checks)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (pull_request_id, observation) DO NOTHING`
	tag, err := db.Exec(ctx, insert, r.PullRequestID, r.HeadCommit, r.Observation(), r.Passed, checks)
	if err != nil {
		return false, fmt.Errorf("recording CI result of pull request %s: %w", r.PullRequestID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// LatestCIResult returns the pull request's latest recorded CI result,
// its CI result as the record states it, and false when none is
// recorded.
func LatestCIResult(ctx context.Context, db DBTX, pullRequestID string) (CIResult, bool, error) {
	if err := checkUUID("pull request ID", pullRequestID); err != nil {
		return CIResult{}, false, fmt.Errorf("reading CI result: %w", err)
	}
	const query = `SELECT head_commit, passed, checks, observed_at FROM pr_ci_results
		WHERE pull_request_id = $1 ORDER BY observed_at DESC, id DESC LIMIT 1`
	r := CIResult{PullRequestID: pullRequestID}
	var checks []byte
	err := db.QueryRow(ctx, query, pullRequestID).Scan(&r.HeadCommit, &r.Passed, &checks, &r.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CIResult{}, false, nil
	}
	if err != nil {
		return CIResult{}, false, fmt.Errorf("reading CI result of pull request %s: %w", pullRequestID, err)
	}
	if err := json.Unmarshal(checks, &r.Checks); err != nil {
		return CIResult{}, false, fmt.Errorf("decoding CI checks of pull request %s: %w", pullRequestID, err)
	}
	r.ObservedAt = r.ObservedAt.UTC()
	return r, true, nil
}
