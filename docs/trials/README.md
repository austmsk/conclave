# Milestone 1 trials

Milestone 1 is done when a labelled issue on the Election Tally repository produces a pull request that passes that repository's CI in at least 7 of 10 trials, with no orphaned sandboxes ([`milestone-1.md`](../tickets/milestone-1.md), ticket 10b). This page says what a trial is, what each one records, and how the report is produced. The trials also seed Milestone 2's replay benchmark (`m1-exit-checklist.md`, item 7).

## Where the files live

The real trial issues and the trial log go in `docs/trials/local/`, which is git-ignored. The Election Tally repository is private and Conclave is public, so anything that describes its code (class names, test names, the issue text) stays off GitHub. This is the same arrangement as `deploy/local/`. Only this page and the report query are tracked.

Suggested layout:

```
docs/trials/local/
  log.md            one row per trial (the table below)
  01-<slug>.md      the issue as filed, one file per trial
  02-<slug>.md
  ...
```

## What a trial is

A trial is one label. The owner files an issue, applies the trigger label, and `/approve`s the plan. That makes one feature request, and the trial is that feature request from label to its final state.

- `/retry` after a stop continues the same trial. It is not a new one.
- A trial ends `delivered` (the pull request's CI passed and the draft was flipped to ready, decision 0014), `needs_attention` (counted as unresolved), or `cancelled`.
- Whether the two required cancellations are 2 of the 10 trials or extra runs on top of them is decided at the start of 10b and recorded in the log's header. The report gives `cancelled` its own count, so it supports either reading.

## The issue format

Every trial issue is written by the owner in the owner-section format. The analyze step parses these sections only when the owner wrote the issue, and it makes no model call before `/approve`:

```markdown
## Task
What to change, in a sentence or two.

## Acceptance criteria
- Each criterion the result must meet, one per line.
- Written so a test can check it.

## Hints
- Optional: files, functions, or approaches worth knowing about.
```

Save the issue exactly as filed, with its title and the base SHA its plan pinned, in that trial's file.

## The trial log

`docs/trials/local/log.md` has one row per trial:

| Field | Where it comes from |
|---|---|
| Trial | 1 to 10, in label order |
| Issue | the issue number and title |
| Base SHA | `plans.base_sha` of the approved plan |
| Attempts | each attempt's ordinal and state, with its reason when it did not pass |
| Gate results | `gate_results` for the delivered attempt: each gate passed, failed or flagged |
| PR | the pull request number and URL |
| CI result | the latest `pr_ci_results` row: passed or failed, and the check names |
| Cost | `cost_usd` from the report |
| Duration | `approve_to_delivered` from the report, plus agent time |
| Notes | what broke, what was fixed and in which commit, `/retry`s, cancellations |

## The report

The report comes from one query, [`internal/store/trial_report.sql`](../../internal/store/trial_report.sql). Its header documents each column. It returns one row per feature request created in a window and is the query Milestone 2's report generator starts from. `store.ReadTrialReport` runs it and adds the summary:

- **Resolve rate:** delivered trials divided by all trials. Cancelled trials are counted beside it.
- **Cost per resolved task:** the spend of every trial in the window divided by the number delivered. The spend on failed trials is charged to the resolved ones, because that is what a resolved task really cost.
- **Time per task:** the mean time from approval to delivery over delivered trials.

Run it through the CLI:

```
conclave report -from 2026-11-01T00:00:00Z -to 2026-12-01T00:00:00Z
```

Or run it in psql 16 or later, connected as a member of `conclave_app` (the query only reads):

```
\i internal/store/trial_report.sql
\bind '2026-11-01T00:00:00Z' '2026-12-01T00:00:00Z' \g
```

Pick a window that holds exactly the ten trials, from just before the first label to just after the last trial ends.

## After the trials

- `store.UnreleasedAttempts` is empty, and `docker ps --filter label=conclave.sandbox` lists nothing.
- The report's resolve rate meets the criterion.
- Copy the report's summary into `log.md`, and update the success-criteria rows in `m1-exit-checklist.md`.
