// Package githubtest holds fakes for testing code that talks to GitHub: a
// fake REST API that checks the App's JWT and hands out canary tokens, a
// WorkflowStarter that behaves like the engine on a duplicate start,
// signed webhook deliveries, a smart-HTTP git server, and an App key
// generated at run time. No test talks to GitHub, and no key or token
// literal appears in the repository.
package githubtest
