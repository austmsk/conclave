// Package activity implements the side effects that Conclave's workflows
// call out to: model requests, sandbox lifecycle, git operations, and
// GitHub API calls.
//
// Every activity is safe to run twice: a retry after a successful-but-
// unreported side effect must not open a second pull request, push twice,
// or record cost twice.
package activity
