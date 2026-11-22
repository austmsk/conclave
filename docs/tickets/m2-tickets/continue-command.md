# Ticket: `/continue` replaces `/retry` as the command the owner types

**Requested by the owner, 2026-11-21:** "retry is confusing". `/retry` does not retry anything: it continues a feature request from `needs_attention` in the phase it left, keeping every attempt and its cost (decision 0009). The word suggests starting over. The owner-facing name becomes `/continue`.

**Requirements:** FR-S11 (commands), FR-SEC10 (every command audited), FR-S12 (the status comment names the commands) · **Review list:** RL-9 (a command not valid in the status gets a reply and changes nothing) · **Decisions to amend:** 0009 and 0013.

## Design: rename what the owner reads and types; keep `retry` on the wire

Each command is its own Temporal signal, named by `workflow.SignalName`, which is the command's string. Every run now waiting in `needs_attention` listens on the `retry` signal. Renaming the signal would need a `workflow.GetVersion` gate, and the webhook server would have to signal both names, since it cannot see a run's version. Renaming the stored value would need a migration of the `commands` CHECK, and would split the audit history across two names.

So the change is at the edges only:

- **The parser accepts `/continue` and `/retry`.** Both map to `github.CommandRetry`. `/continue` is the documented name; `/retry` is an alias for the owner's habit and for old comments, and is never shown.
- **The wire name stays `retry`:** the constant's value, the signal name, the value stored in `commands.command`, and the activity and workflow identifiers. That way no migration, no version gate and no replay fixture change is needed.
- **Every owner-facing string says `/continue`:** the `/help` reply, the status comment ("**Left from:** …; `/continue` continues there", "**`/continue` refused:** …"), the "not valid in this status" replies, and the `pause` alert's owner action (`internal/alert/alert.go`).
- **The audit row records what the owner typed.** It still stores `retry` as the command, so queries and history stay continuous. If the record should also keep the spelling typed, that is a new column, so propose it and stop rather than adding it; it is not required for this ticket.
- **The code says why the names differ.** The comment on `CommandRetry`, and on `SignalName`, states that `retry` is the wire name kept for replay and audit continuity, and `/continue` is what the owner types. A reader must not be left to guess.

Do not rename `CommandRetry` or the Go identifiers in this ticket. A full internal rename can be a later decision, if the split proves confusing in practice.

## Where it changes

| Area | Files |
|---|---|
| Parser | `internal/github/command.go`: `commandLine` accepts `continue`, both spellings map to `CommandRetry`, the doc comment |
| Owner-facing text | `internal/workflow/signals.go` (`/help` and refusal replies), `internal/activity/status.go`, `internal/activity/caps.go`, `internal/alert/alert.go` |
| Comments that name the command | `internal/workflow/retry.go`, `feature.go`, `delivery.go`, `types.go`; `internal/activity/delivery.go`, `task.go`, `types.go`; `internal/store/commands.go`. Owner-facing wording becomes `/continue`; comments about the wire name keep `retry` |
| Docs, current tense | `ARCHITECTURE.md` (command table and the paragraph under it), `docs/scope-and-requirements.md` (FR-S11 and any FR naming `/retry`), `docs/runbooks.md`, `docs/trials/README.md`, `docs/guide/glossary.md`, `docs/guide/overview.md`, and the guide pages for tickets 8, 8a, 8b, 9 and 10b |
| Decisions | Dated amendments to 0009 (the command that continues is now `/continue`) and 0013 (the command surface: the alias, and why the wire name stays). Mentions of `/retry` in 0014 and 0015 stay as written, history, with 0013's amendment as the pointer |

Historical documents (`docs/tickets/m1-tickets/`, the trial log) stay as written.

## Tests

| Behaviour | Test |
|---|---|
| `/continue` and `/retry` parse to the same command, with and without trailing text; `/continued`, `/continue-now` and `/Continue` do not parse | github `TestParseCommand`, new cases |
| A command from anyone but the owner is still ignored under either spelling | github, the existing owner-only tests with a `/continue` case |
| `/help` lists `/continue` and never `/retry`, in the table's order | workflow `TestHelpListsValidCommands`, `TestValidCommandsOrder` |
| `/continue` outside `needs_attention` gets the "not valid" reply naming `/continue` and changes nothing (RL-9) | workflow `TestInvalidCommandPerLiveStatus`, `TestCommandApplies` |
| The status comment and the cap-refusal text name `/continue` | activity `TestStatusCommentNamesReason`, `TestStatusCommentNamesCap` |
| The `pause` alert's owner action names `/continue` | alert, the existing budget-cap alert test |
| A `/continue` is audited with command `retry` and the owner's actor | workflow `TestEveryCommandAudited`, a `/continue` case |
| Runs started before the change still replay | workflow `TestReplayMidAttempt`, `TestReplayDetectsRerun`, unchanged fixtures |

## Done when

- The owner can type `/continue` on an issue in `needs_attention` and the run continues, exactly as `/retry` did.
- `/retry` still works, and nothing the platform writes says it.
- No migration, no change to `workflow.SignalName`, no `GetVersion`, no change to the replay fixtures.
- The docs and decisions above are updated. The ticket's guide page is written from `docs/guide/tickets/_template.md`, and `make check` passes.
- The PR lists each requirement ID with its test, and says how RL-9 still holds.
