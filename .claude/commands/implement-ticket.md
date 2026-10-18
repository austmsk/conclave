---
description: Implement one ticket from docs/tickets/
---

Implement ticket $ARGUMENTS.

1. Read the ticket in `docs/tickets/`. Read the requirement text for every `FR-*` / `NFR-*` it cites and every `RL-*` review-list item it names.
2. Create a branch: `git checkout -b ticket/$ARGUMENTS`.
3. Implement only what the ticket covers. If it appears to require changes outside its stated files — especially to `internal/contracts/` — stop and say so.
4. Write tests that fail when the implementation is wrong. For any security control, write a test that attempts the attack.
5. Run `make check`. Do not report the ticket complete until it passes.
6. Write the ticket's guide page: copy `docs/guide/tickets/_template.md` to `docs/guide/tickets/NN-short-name.md` and fill it in from the code as written, not from the ticket text. Every test it cites must exist. Mark the ticket in `docs/guide/tickets/README.md`, and update `docs/guide/overview.md` if the ticket changes a step in a request's path.
7. Commit with a Conventional Commits message referencing the ticket.

Then summarise: what changed, how each acceptance criterion is met, how each named review-list item is satisfied, what the guide page covers, and anything you were uncertain about.
