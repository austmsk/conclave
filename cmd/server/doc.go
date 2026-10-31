// Command server is the internet-facing process. It serves GitHub's
// webhook at POST /webhooks/github and a health check at GET /healthz, and
// nothing else.
//
// It reads the GitHub App configuration (-github-config, deploy/github.yaml)
// and takes the enabled repositories from the repository access file
// (-repos, deploy/repos.yaml). It reads the webhook secret from
// CONCLAVE_GITHUB_WEBHOOK_SECRET, set by the owner's secrets file, and
// unsets the variable once read. It never loads the App's private key:
// that belongs to the worker, so this process can verify deliveries and
// ask for workflows to start, and do nothing else on GitHub (FR-SEC2,
// FR-SEC5).
//
// Accepted deliveries are acknowledged at once and dispatched to Temporal
// (-temporal-address, -temporal-namespace) from a queue (NFR-P1): a label
// starts the issue's feature workflow under its derived ID, refusing a
// duplicate, and a command is signalled on its own name. On SIGINT or
// SIGTERM the server stops accepting connections, lets in-flight requests
// finish, then drains the queue, logging the delivery ID of anything it
// could not dispatch so the owner can redeliver it.
package main
