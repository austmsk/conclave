package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/store"
)

const (
	prBase   = "0123456789abcdef0123456789abcdef01234567"
	prLanded = "1111111111111111111111111111111111111111"
	prOther  = "2222222222222222222222222222222222222222"
	prHash   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	prHash2  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func newPullRequest(s ids) store.PullRequestRecord {
	return store.PullRequestRecord{
		FeatureRequestID: s.feature, TaskID: s.task, AttemptID: s.attempt,
		IntegrationBranch: "agent/7/integration", PRBranch: "agent/7/pr-1",
		OriginalBaseSHA: prBase, OriginalArtifactHash: prHash,
		Title: "Export tallies as CSV", Summary: "Adds an exporter.", Risks: "None known.",
		CommitMessage: "feat(export): add CSV export\n\nTask: T1\nGenerated-by: conclave\n",
	}
}

// seedPullRequest creates the seed attempt's pull request record.
func seedPullRequest(t *testing.T, ctx context.Context, tx pgx.Tx) (ids, store.PullRequestRecord) {
	t.Helper()
	s := seed(t, ctx, tx)
	pr, created, err := store.CreatePullRequest(ctx, tx, newPullRequest(s))
	if err != nil || !created {
		t.Fatalf("CreatePullRequest = %v, created %v", err, created)
	}
	return s, pr
}

func TestCreatePullRequestIdempotent(t *testing.T) {
	ctx, tx := testTx(t)
	s, first := seedPullRequest(t, ctx, tx)
	again := newPullRequest(s)
	again.Title, again.CommitMessage = "Another title", "fix: something else\n"
	second, created, err := store.CreatePullRequest(ctx, tx, again)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("second CreatePullRequest reported created")
	}
	if second != first {
		t.Errorf("second call returned %+v, want the first row %+v", second, first)
	}
	if first.Title != "Export tallies as CSV" || first.LandedCommit != "" || first.Ready {
		t.Errorf("stored row %+v", first)
	}
}

func TestCreatePullRequestRefusesBadInput(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	cases := map[string]func(*store.PullRequestRecord){
		"bad attempt ID":  func(r *store.PullRequestRecord) { r.AttemptID = "x" },
		"short base":      func(r *store.PullRequestRecord) { r.OriginalBaseSHA = "abc123" },
		"prefixed hash":   func(r *store.PullRequestRecord) { r.OriginalArtifactHash = "sha256:" + prHash },
		"blank title":     func(r *store.PullRequestRecord) { r.Title = "  " },
		"no message":      func(r *store.PullRequestRecord) { r.CommitMessage = "" },
		"NUL in summary":  func(r *store.PullRequestRecord) { r.Summary = "a\x00b" },
		"invalid UTF-8":   func(r *store.PullRequestRecord) { r.Risks = "\xff" },
		"bad branch name": func(r *store.PullRequestRecord) { r.PRBranch = "main" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			sp := savepoint(t, ctx, tx)
			rec := newPullRequest(s)
			mutate(&rec)
			if _, _, err := store.CreatePullRequest(ctx, sp, rec); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestPullRequestForAttemptNotFound(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	if _, err := store.PullRequestForAttempt(ctx, tx, s.attempt); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestSetPullRequestValue(t *testing.T) {
	cases := []struct {
		name          string
		column, first string
		second        string
		wantErr       error
		wantAnyErr    bool
	}{
		{name: "same value again is a no-op", column: store.ColLandedCommit, first: prLanded, second: prLanded},
		{name: "another value is refused", column: store.ColBaseSHA, first: prLanded, second: prOther, wantErr: store.ErrAlreadyRecorded},
		{name: "rebased hash write-once", column: store.ColRebasedArtifactHash, first: prHash, second: prHash2, wantErr: store.ErrAlreadyRecorded},
		{name: "head commit write-once", column: store.ColHeadCommit, first: prLanded, second: prOther, wantErr: store.ErrAlreadyRecorded},
		{name: "malformed commit", column: store.ColLandedCommit, second: "HEAD", wantAnyErr: true},
		{name: "commit in a hash column", column: store.ColRebasedArtifactHash, second: prLanded, wantAnyErr: true},
		{name: "unknown column", column: "title", second: prLanded, wantAnyErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, tx := testTx(t)
			_, pr := seedPullRequest(t, ctx, tx)
			if tc.first != "" {
				if err := store.SetPullRequestValue(ctx, tx, pr.ID, tc.column, tc.first); err != nil {
					t.Fatalf("first set: %v", err)
				}
			}
			err := store.SetPullRequestValue(ctx, tx, pr.ID, tc.column, tc.second)
			switch {
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			case tc.wantAnyErr && err == nil:
				t.Fatal("accepted")
			case tc.wantErr == nil && !tc.wantAnyErr && err != nil:
				t.Fatalf("got %v, want nil", err)
			}
			// A refusal before SQL leaves the transaction usable.
			if _, err := store.PullRequestForAttempt(ctx, tx, pr.AttemptID); err != nil {
				t.Fatalf("transaction unusable after the refusal: %v", err)
			}
		})
	}
}

func TestSetPullRequestValueUnknownRecord(t *testing.T) {
	ctx, tx := testTx(t)
	err := store.SetPullRequestValue(ctx, tx, "00000000-0000-4000-8000-000000000000", store.ColLandedCommit, prLanded)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestSetPullRequestOpened(t *testing.T) {
	ctx, tx := testTx(t)
	_, pr := seedPullRequest(t, ctx, tx)
	if err := store.SetPullRequestOpened(ctx, tx, pr.ID, 3, "PR_3", "https://github.example/pull/3"); err == nil {
		t.Fatal("recorded a pull request before its head commit")
	}
	if err := store.SetPullRequestValue(ctx, tx, pr.ID, store.ColHeadCommit, prLanded); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.SetPullRequestOpened(ctx, tx, pr.ID, 3, "PR_3", "https://github.example/pull/3"); err != nil {
			t.Fatalf("SetPullRequestOpened: %v", err)
		}
	}
	err := store.SetPullRequestOpened(ctx, tx, pr.ID, 4, "PR_4", "https://github.example/pull/4")
	if !errors.Is(err, store.ErrAlreadyRecorded) {
		t.Fatalf("second pull request: got %v, want ErrAlreadyRecorded", err)
	}
	if err := store.SetPullRequestOpened(ctx, tx, pr.ID, 0, "", ""); err == nil {
		t.Fatal("accepted an empty pull request")
	}
	got, err := store.PullRequestForAttempt(ctx, tx, pr.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Number != 3 || got.NodeID != "PR_3" || got.URL != "https://github.example/pull/3" {
		t.Errorf("stored %+v", got)
	}
}

func TestMarkPullRequestReady(t *testing.T) {
	ctx, tx := testTx(t)
	_, pr := seedPullRequest(t, ctx, tx)
	if err := store.MarkPullRequestReady(ctx, tx, pr.ID); err == nil {
		t.Fatal("marked ready a pull request never opened")
	}
	if err := store.SetPullRequestValue(ctx, tx, pr.ID, store.ColHeadCommit, prLanded); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPullRequestOpened(ctx, tx, pr.ID, 3, "PR_3", "https://github.example/pull/3"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.MarkPullRequestReady(ctx, tx, pr.ID); err != nil {
			t.Fatalf("MarkPullRequestReady: %v", err)
		}
	}
	got, err := store.PullRequestForAttempt(ctx, tx, pr.AttemptID)
	if err != nil || !got.Ready {
		t.Fatalf("ready = %v, %v", got.Ready, err)
	}
	if err := store.MarkPullRequestReady(ctx, tx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown record: got %v, want ErrNotFound", err)
	}
}

func TestRecordCIResult(t *testing.T) {
	ctx, tx := testTx(t)
	_, pr := seedPullRequest(t, ctx, tx)
	if _, found, err := store.LatestCIResult(ctx, tx, pr.ID); err != nil || found {
		t.Fatalf("before any result: found %v, %v", found, err)
	}
	failed := store.CIResult{PullRequestID: pr.ID, HeadCommit: prLanded, Passed: false,
		Checks: []store.CICheck{{Name: "build", ID: 10, Conclusion: "failure"}}}
	for i, want := range []bool{true, false} {
		wrote, err := store.RecordCIResult(ctx, tx, failed)
		if err != nil || wrote != want {
			t.Fatalf("record %d: wrote %v, %v; want %v", i, wrote, err, want)
		}
	}
	rerun := store.CIResult{PullRequestID: pr.ID, HeadCommit: prLanded, Passed: true,
		Checks: []store.CICheck{{Name: "build", ID: 11, Conclusion: "success"}}}
	if wrote, err := store.RecordCIResult(ctx, tx, rerun); err != nil || !wrote {
		t.Fatalf("re-run: wrote %v, %v", wrote, err)
	}
	checkLatestCI(t, ctx, tx, pr.ID, 2, rerun)
}

// checkLatestCI checks the pull request holds rows CI results and the
// latest is want.
func checkLatestCI(t *testing.T, ctx context.Context, tx pgx.Tx, prID string, rows int, want store.CIResult) {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pr_ci_results WHERE pull_request_id = $1`, prID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != rows {
		t.Errorf("%d rows, want %d", n, rows)
	}
	latest, found, err := store.LatestCIResult(ctx, tx, prID)
	if err != nil || !found {
		t.Fatalf("latest: found %v, %v", found, err)
	}
	if latest.Passed != want.Passed || len(latest.Checks) != 1 || latest.Checks[0] != want.Checks[0] || latest.HeadCommit != want.HeadCommit {
		t.Errorf("latest = %+v, want %+v", latest, want)
	}
}

func TestRecordCIResultRefusesBadInput(t *testing.T) {
	ctx, tx := testTx(t)
	_, pr := seedPullRequest(t, ctx, tx)
	cases := map[string]store.CIResult{
		"no checks":      {PullRequestID: pr.ID, HeadCommit: prLanded},
		"short commit":   {PullRequestID: pr.ID, HeadCommit: "abc", Checks: []store.CICheck{{Name: "b"}}},
		"bad ID":         {PullRequestID: "x", HeadCommit: prLanded, Checks: []store.CICheck{{Name: "b"}}},
		"NUL check name": {PullRequestID: pr.ID, HeadCommit: prLanded, Checks: []store.CICheck{{Name: "b\x00"}}},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := store.RecordCIResult(ctx, tx, r); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestCIObservationKey(t *testing.T) {
	a := store.CIResult{HeadCommit: prLanded, Checks: []store.CICheck{{Name: "build", ID: 1, Conclusion: "success"}}}
	b := a
	b.Checks = []store.CICheck{{Name: "build", ID: 2, Conclusion: "success"}}
	c := a
	c.Checks = []store.CICheck{{Name: "build", ID: 1, Conclusion: "failure"}}
	same := a
	same.Checks = []store.CICheck{{Name: "build", ID: 1, Conclusion: "success"}}
	if a.Observation() != same.Observation() {
		t.Error("observation is not stable")
	}
	if a.Observation() == b.Observation() || a.Observation() == c.Observation() {
		t.Error("a re-run or another conclusion gave the same observation")
	}
}

// TestAppRoleCannotRewritePullRequests attempts the rewrite a compromised
// worker would make: changing what a recorded pull request says it
// delivered, or erasing its CI history. The application role may fill
// only the step columns, and pr_ci_results is append-only.
func TestAppRoleCannotRewritePullRequests(t *testing.T) {
	ctx, tx := testTx(t)
	_, pr := seedPullRequest(t, ctx, tx)
	if err := store.SetPullRequestValue(ctx, tx, pr.ID, store.ColHeadCommit, prLanded); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCIResult(ctx, tx, store.CIResult{PullRequestID: pr.ID, HeadCommit: prLanded,
		Checks: []store.CICheck{{Name: "build", ID: 1, Conclusion: "failure"}}}); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, sql, want string }{
		{"update title", `UPDATE pull_requests SET title = 'forged'`, permissionDenied},
		{"update commit message", `UPDATE pull_requests SET commit_message = 'forged'`, permissionDenied},
		{"update original artifact", `UPDATE pull_requests SET original_artifact_hash = '` + prHash2 + `'`, permissionDenied},
		{"delete pull request", `DELETE FROM pull_requests`, permissionDenied},
		{"update CI result", `UPDATE pr_ci_results SET passed = true`, permissionDenied},
		{"delete CI result", `DELETE FROM pr_ci_results`, permissionDenied},
		{"truncate CI results", `TRUNCATE pr_ci_results`, permissionDenied},
		{"rewrite a step column", `UPDATE pull_requests SET head_commit = '` + prOther + `'`, integrityViolation},
		{"fill an empty step column", `UPDATE pull_requests SET landed_commit = '` + prLanded + `'`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := asAppRole(t, ctx, tx, func(sp pgx.Tx) error {
				_, err := sp.Exec(ctx, tc.sql)
				return err
			})
			if got := sqlState(err); got != tc.want {
				t.Fatalf("got error %v (SQLSTATE %q), want SQLSTATE %q", err, got, tc.want)
			}
		})
	}
}

// TestPullRequestWriteOnceHoldsForOwner: the trigger, not the grant,
// keeps a recorded step column fixed, so even the table owner cannot
// point a delivered pull request at another commit.
func TestPullRequestWriteOnceHoldsForOwner(t *testing.T) {
	ctx := requireDB(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	_, pr := seedPullRequest(t, ctx, tx)
	if err := store.SetPullRequestValue(ctx, tx, pr.ID, store.ColLandedCommit, prLanded); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, sql string }{
		{"landed commit", `UPDATE pull_requests SET landed_commit = '` + prOther + `'`},
		{"landed commit to null", `UPDATE pull_requests SET landed_commit = NULL`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := savepoint(t, ctx, tx)
			_, err := sp.Exec(ctx, tc.sql)
			if got := sqlState(err); got != integrityViolation {
				t.Fatalf("got %v (SQLSTATE %q), want %q", err, got, integrityViolation)
			}
			if err == nil || !strings.Contains(err.Error(), "write-once") {
				t.Fatalf("error %v does not name the write-once rule", err)
			}
		})
	}
}
