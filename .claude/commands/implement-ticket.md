---
description: Implement one ticket from docs/tickets/
---

Implement ticket $ARGUMENTS.

1. Read the ticket in `docs/tickets/`. Read the requirement text for every `FR-*` / `NFR-*` it cites and every `RL-*` review-list item it names.
2. Create a branch: `git checkout -b ticket/$ARGUMENTS`.
3. Implement only what the ticket covers. If it appears to require changes outside its stated files — especially to `internal/contracts/` — stop and say so.
4. Write tests that fail when the implementation is wrong. For any security control, write a test that attempts the attack.
5. Run `make check`. Do not report the ticket complete until it passes.
6. Commit with a Conventional Commits message referencing the ticket.

Then summarise: what changed, how each acceptance criterion is met, how each named review-list item is satisfied, and anything you were uncertain about.
