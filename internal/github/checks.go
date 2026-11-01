package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// CheckRun is one check run on a commit, as the checks API reports it.
// Status is queued, in_progress or completed; Conclusion is set once it
// has completed. AppSlug names the App that created the run, so a caller
// can refuse a run with the right name from the wrong source.
type CheckRun struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
	AppSlug    string
}

const (
	// checkRunsPerPage is the page size asked for, GitHub's largest.
	checkRunsPerPage = 100
	// maxCheckRunPages bounds how many pages are read for one commit, so a
	// commit with runaway check runs cannot hold an activity for long.
	maxCheckRunPages = 10
)

type checkRunsReply struct {
	TotalCount int `json:"total_count"`
	CheckRuns  []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		App        struct {
			Slug string `json:"slug"`
		} `json:"app"`
	} `json:"check_runs"`
}

// ListCheckRuns returns every check run on commit, up to
// maxCheckRunPages pages, in the order GitHub lists them. commit must be
// a full commit ID: anything else is refused before a token is minted,
// so a branch name cannot make the platform read whatever a branch
// points at. It needs the checks: read permission (decision 0014).
func (a *App) ListCheckRuns(ctx context.Context, repo RepoRef, commit string) ([]CheckRun, error) {
	if err := checkFullName(repo.FullName); err != nil {
		return nil, err
	}
	if !commitID.MatchString(commit) {
		return nil, fmt.Errorf("%w: commit %q is not a full commit ID", ErrRefused, commit)
	}
	tok, err := a.token(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("github: listing check runs: %w", err)
	}
	var runs []CheckRun
	for page := 1; page <= maxCheckRunPages; page++ {
		q := url.Values{"per_page": {strconv.Itoa(checkRunsPerPage)}, "page": {strconv.Itoa(page)}}
		var reply checkRunsReply
		path := repoPath(repo, "/commits/%s/check-runs?%s", commit, q.Encode())
		if err := a.send(ctx, repo, tok, "listing check runs", http.MethodGet, path, nil, &reply); err != nil {
			return nil, err
		}
		for _, r := range reply.CheckRuns {
			runs = append(runs, CheckRun{ID: r.ID, Name: r.Name, Status: r.Status, Conclusion: r.Conclusion, AppSlug: r.App.Slug})
		}
		if len(reply.CheckRuns) < checkRunsPerPage || len(runs) >= reply.TotalCount {
			break
		}
	}
	return runs, nil
}
