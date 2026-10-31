# Decision records

One record per non-obvious choice, written after the ticket merges ([0001](0001-record-decisions.md), `NFR-M9`). A `proposed` record has been drafted but not yet accepted by the owner.

| # | Decision | Status |
|---|---|---|
| [0001](0001-record-decisions.md) | Record decisions | accepted |
| 0002 | *Reserved: ticket 2, audit trail and state transitions* | not written |
| 0003 | *Reserved: ticket 2, new dependencies (pgx, goose, testcontainers-go)* | not written |
| [0004](0004-exec-on-sandbox-mounts.md) | Sandbox mounts allow exec | accepted |
| 0005 | *Reserved: Jev as a decision provider ([`jev.md`](../tickets/jev.md))* | not written |
| [0006](0006-prompt-version-on-model-requests.md) | Model requests carry a prompt version | accepted |
| [0007](0007-yaml-library.md) | `go.yaml.in/yaml/v3` for configuration | accepted |
| [0008](0008-provider-adapters-on-net-http.md) | Provider adapters are written on `net/http`, not vendor SDKs | proposed |
| [0009](0009-budget-caps-route-to-needs-attention.md) | A stopped feature request goes to `needs_attention`, and `/retry` continues it | proposed |
| [0010](0010-github-client-on-net-http.md) | GitHub App auth and REST are written on `net/http`, not `go-github` or `ghinstallation` | proposed |
| [0011](0011-sandbox-release-by-id.md) | Sandboxes are released by ID, cancellation is a reason, and the trial image is built locally | proposed |
| [0012](0012-temporal-workflow-shape.md) | The Temporal Go SDK, one activity per attempt, and the attempt timeout | proposed |
| [0013](0013-commands-and-status-surface.md) | Commands and the status surface: one table, a closed-run workflow, and a versioned change | proposed |

New records take the next free number, and this table gains a row in the same change.
