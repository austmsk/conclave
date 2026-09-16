// Package github implements GitHub App authentication, webhook handling,
// and pull request operations.
//
// Agents never hold a GitHub credential or a network path; platform code
// in this package is what pushes branches, opens pull requests, posts
// comments, and edits them in place, after checks pass.
package github
