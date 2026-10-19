// Package github is the platform's only path to GitHub: it receives
// webhooks, decides who may command a run, and performs the repository
// writes, pushing agent/ branches, opening pull requests, posting and
// editing comments, and adding reactions (FR-I1, FR-SEC5).
//
// Agents never hold a GitHub credential or a network path. The two halves
// of this package run in different processes and hold different secrets.
// The webhook Receiver runs in the internet-facing server and holds only
// the webhook secret: it verifies every delivery's HMAC before parsing it
// (FR-SEC2), honours commands only from the owner's numeric user ID, and
// never from an App or bot (FR-SEC1, FR-SEC28), and hands accepted work to
// a WorkflowStarter without waiting on it (NFR-P1). The App client runs in
// the worker and alone holds the App's private key; it mints installation
// tokens scoped to one repository and a fixed permission set that never
// includes workflows or administration (FR-SEC15), and pushes only to
// agent/ branches with a lease (FR-SEC16). A compromised receiver can at
// most ask for a workflow to start.
//
// Credentials never reach a log, an error, or a trace (RL-8). They are held
// in a type that prints as [redacted], errors carry fixed text and an HTTP
// status rather than GitHub's message, and redirects are never followed.
//
// Text that came from GitHub reaches an agent only through Quarantine,
// which wraps it in a nonce-delimited block labelled with a provenance
// taken from the payload's author fields, never from the text itself.
package github
