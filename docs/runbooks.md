# Runbooks

What to do when Conclave raises an alert, and how to use the kill switch. Each alert issue in the operations repository links to its section here ([`internal/alert`](../internal/alert), FR-SEC31, decision [0015](decisions/0015-kill-switch-alerts-and-reaper.md)). For a suspected compromise, follow the [incident response runbook](security-and-threat-model.md#incident-response-runbook), whose first step is the halt below.

Every `conclave` command runs on the central machine and needs:

- `CONCLAVE_DATABASE_URL` and `CONCLAVE_GITHUB_APP_KEY_FILE`, from the secrets file outside the repository. The CLI reads each once and unsets it.
- `-github-config deploy/local/github.yaml`, with its `alerts:` block filled in. Without one the CLI refuses to start.
- `-alert-log <path>` for `halt` and `alert-test`: the same local alert log the worker writes.
- `-temporal-address` and `-temporal-namespace`, if not the defaults.
- Docker access, for the sweep.

## Halt

**When:** you suspect a compromise, an agent is doing something you didn't ask for, or spend is running away faster than the caps catch. A halt is safe to run as a drill.

```
conclave halt -reason "why, in a few words" -github-config deploy/local/github.yaml -alert-log <path>
```

`conclave halt` does the following, in order. Every step is safe to repeat, so running `halt` a second time continues the first halt rather than starting another.

1. **Records the halt** as a `set` row in `halt_events`. This happens before anything else, in Postgres rather than in Temporal or on GitHub, so it holds even when either of those is the problem. A second `halt` while halted keeps the first row and its time.
2. **Raises the halt alert.** It is written to the local alert log first, then opened as an issue in the operations repository. If delivery fails, the failure is recorded in the log and the halt goes on.
3. **Pauses the `conclave-reaper` schedule.**
4. **Cancels every open workflow.** Only root workflows are cancelled: a feature workflow cancels its own task child and releases its sandbox from a disconnected context. The CLI lists open workflows again and cancels any new ones until two listings in a row, a second apart, find none open, or `-drain` (3 minutes by default) runs out. That catches a label applied during the halt, even though Temporal's listing trails a new start by a moment.
5. **Sweeps every sandbox with no age cutoff.** Each recorded, unreleased sandbox is released, and every unrecorded sandbox container in the worker's namespace is removed. A sandbox still in use is removed too: that is the point of a halt.
6. **Checks what is left:** attempts with a sandbox and no `sandbox_released_at`, and containers carrying the `conclave.sandbox` label. It prints both counts.
7. **Comments on the halt's issue** with what it did: workflows cancelled and still open, sandboxes released and reaped, and what remains.

Each worker polls `halt_events` every 5 seconds. When it sees the halt it keeps running for `-halt-grace` (3 minutes by default), which is longer than the attempt heartbeat timeout, so the cancellations it receives can finish their cleanup. Then it exits with "halted by the kill switch". A worker refuses to start while the halt stands.

**Check afterwards:**

- The halt comment says nothing is still open and nothing remains. If something does, run `conclave halt` again. Then check by hand:
  - `docker ps -a --filter label=conclave.sandbox`
  - `SELECT id, sandbox_id FROM attempts WHERE sandbox_id IS NOT NULL AND sandbox_released_at IS NULL;`
- Every worker process has exited.
- The Temporal UI shows no running workflow. A workflow still open is waiting on a cleanup activity no worker is left to run; it finishes after resume.

**Resume** when it is safe to start work again:

```
conclave resume -reason "why" [-keep] -github-config deploy/local/github.yaml
```

`resume` lists every workflow started after the halt's time. These are labels applied during the halt, which the webhook server, having no database access, could not refuse. It cancels them unless you pass `-keep`, so nothing you didn't see starts on its own. It then records a `clear` row and unpauses the reaper schedule. Start the workers again afterwards. To run an issue that was cancelled this way, remove the trigger label and apply it again.

## Budget cap

**When:** an alert titled "Budget cap: a feature request is paused". A model call was refused because the attempt, feature request or monthly cap (FR-SEC9) would have been passed. The feature request is in `needs_attention` with reason `budget_cap:<kind>`. Its attempts and their recorded spend are kept.

1. Read the alert's **Where** line. It gives the repository, issue, feature request and workflow. The issue's status comment shows the same stop.
2. Decide whether the spend is expected. The query below lists the feature request's cost events:
   ```
   SELECT role, model, input_tokens, output_tokens, cost_usd, occurred_at
   FROM cost_events WHERE feature_request_id = '<id>' ORDER BY occurred_at;
   ```
3. If the spend is expected, raise the cap the alert names in `deploy/local/router.yaml` (`budgets:`), restart the worker, and comment `/retry` on the issue. `/retry` checks the caps first. If the run is still over a cap, the reply names the cap and nothing changes.
4. If not, comment `/cancel`.

A second stop of the same feature request, after a `/retry`, comments on the same open issue. Close the issue once it is dealt with; a later stop then opens a new one.

## Alert delivery failed

**When:** a `conclave` command printed a delivery error, the worker logged "raising the budget cap alert", or an alert you expected never arrived. The alert is still in the local log, which is written before delivery is tried.

1. Read the local log. It holds one JSON object per line:
   - an `alert` entry, with the whole alert;
   - then a `delivery` entry, with `outcome` `delivered` (and the issue number) or `failed` (and the error).

   A retried delivery may write the same occurrence twice, so group entries by occurrence:
   ```
   jq -c 'select(.type == "delivery" and .outcome == "failed")' <path>
   ```
2. Check the GitHub side:
   - The operations repository is in the App's installation.
   - `alerts.repository_id` in `github.yaml` is its numeric ID.
   - The installation grants `issues: write`. A token for the operations repository asks for issues and metadata only, and any wider token is discarded unused.
   - Watch for `HTTP 401`: a rejected key or a revoked installation.
3. Act on the alert from the log itself if GitHub is down or suspect. A halt does not depend on delivery.
4. Retrying the same occurrence is safe: it finds its issue or comment and posts nothing twice. For a budget cap, the worker retries for up to 10 minutes and then gives up, and the issue's status comment still shows the stop. For a halt, `conclave halt` again redelivers it.

**Make the log append-only for its owner too.** By default the file is opened only for appending, with mode 0600, and never through a symlink, but its owner could still truncate it.

- macOS: `chflags uappnd <path>`
- Linux: `sudo chattr +a <path>`

To rotate or move the log, clear the flag first (`chflags nouappnd`, or `chattr -a`), then copy the file aside and start a new one.

## Alert test

**When:** after setting up the operations repository, or to check that alerts still arrive.

```
conclave alert-test -github-config deploy/local/github.yaml -alert-log <path>
```

This raises a `digest` alert titled "Alert test". The local log gains an `alert` entry and a `delivery` entry, and an issue opens in the operations repository, labelled `conclave-alert` and `severity:digest` and assigned to you. Running it again while that issue is open adds a comment rather than a second issue, which shows deduplication works. Close the issue when you are done.
