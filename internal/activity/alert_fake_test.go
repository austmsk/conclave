package activity

import (
	"context"
	"fmt"
	"sync"

	"github.com/austmsk/conclave/internal/github"
)

// fakeAlertIssues is the operations repository's issues, in memory: what
// *github.App's alert calls would leave there.
type fakeAlertIssues struct {
	mu       sync.Mutex
	issues   []github.Issue
	comments map[int][]github.Comment
	// fail, when set, is returned by every call.
	fail error
}

func newFakeAlertIssues() *fakeAlertIssues {
	return &fakeAlertIssues{comments: map[int][]github.Comment{}}
}

func (f *fakeAlertIssues) CreateAlertIssue(_ context.Context, _, body string, _ []string) (github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return github.Issue{}, f.fail
	}
	is := github.Issue{Number: len(f.issues) + 1, Body: body}
	is.URL = fmt.Sprintf("https://github.example/ops/issues/%d", is.Number)
	f.issues = append(f.issues, is)
	return is, nil
}

func (f *fakeAlertIssues) ListOpenAlertIssues(context.Context, string) ([]github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	return append([]github.Issue(nil), f.issues...), nil
}

func (f *fakeAlertIssues) CommentOnAlertIssue(_ context.Context, issue int, body string) (github.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return github.Comment{}, f.fail
	}
	c := github.Comment{ID: int64(len(f.comments[issue]) + 1), Body: body}
	f.comments[issue] = append(f.comments[issue], c)
	return c, nil
}

func (f *fakeAlertIssues) AlertIssueComments(_ context.Context, issue int) ([]github.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	return append([]github.Comment(nil), f.comments[issue]...), nil
}

// counts reports how many alert issues and comments there are.
func (f *fakeAlertIssues) counts() (issues, comments int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, cs := range f.comments {
		comments += len(cs)
	}
	return len(f.issues), comments
}
