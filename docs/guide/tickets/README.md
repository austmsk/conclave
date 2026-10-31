# Tickets

One page per merged ticket, in build order. The spec for each ticket is in [`docs/tickets/milestone-1.md`](../../tickets/milestone-1.md). These pages describe what the merged code actually does.

| # | Ticket | Status | Page |
|---|---|---|---|
| 1 | Repository skeleton and CI | merged | [01-skeleton](01-skeleton.md) |
| 2 | Contracts package and schema | merged | [02-contracts-and-schema](02-contracts-and-schema.md) |
| 3 | Docker sandbox | merged | [03-docker-sandbox](03-docker-sandbox.md) |
| 4 | LLM router and fake provider | merged (PR #4) | [04-llm-router](04-llm-router.md) |
| 8a | Budgets and attribution | merged (PR #10) | [08a-budgets-and-attribution](08a-budgets-and-attribution.md) |
| 5 | Agent loop and tools | merged (PR #11) | [05-agent-loop-and-tools](05-agent-loop-and-tools.md) |
| 6 | GitHub App and webhooks | merged (PR #12) | [06-github-app-and-webhooks](06-github-app-and-webhooks.md) |
| 7 | Gates and artifact integrity | merged (PR #15) | [07-gates-and-artifact-integrity](07-gates-and-artifact-integrity.md) |
| 8-pre | Workflow prerequisites | merged (PR #19) | [08-pre-workflow-prerequisites](08-pre-workflow-prerequisites.md) |
| 8 | Feature workflow core | merged (PR #20) | [08-feature-workflow-core](08-feature-workflow-core.md) |
| 8b | Commands and status surface | in review | [08b-commands-and-status-surface](08b-commands-and-status-surface.md) |
| 9 | Pull request delivery | not started | — |
| 10 | End-to-end run and hardening | not started | — |

The order is the working order: 8a runs before 5 (see `milestone-1.md`).

To add a page, copy [`_template.md`](_template.md) to `NN-short-name.md` and update this table.
