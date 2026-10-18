# Guide

What Conclave does and how each ticket built it, written to be read and re-read. The rest of `docs/` says what the system *must* do (requirements, threat model, tickets). This folder explains what it *does*, in plain terms, with pointers into the code.

## Reading order

1. [`overview.md`](overview.md): how a labeled issue becomes a pull request, step by step, and which package does each step. Start here.
2. [`glossary.md`](glossary.md): the project's vocabulary (tier, scope, attempt, gate, compare-and-set, and so on).
3. [`tickets/`](tickets/): one page per merged ticket. It covers what was built and why, how it works, where the code is, what the tests prove, and what was left open.

Read a ticket page next to its code. Each page ends with questions. If you can answer them without looking, you understand the ticket. If you can't, the page says where to look.

## How this differs from the other docs

| Folder | Says | Changes when |
|---|---|---|
| `docs/tickets/` | What a ticket must build (the spec) | The plan changes |
| `docs/decisions/` | Why one non-obvious choice was made, in the owner's words | A decision is made |
| `docs/guide/` | How the system works now, ticket by ticket | A ticket merges |

A guide page can link to a decision record, but it doesn't replace one.

## Keeping it current

Each ticket writes its page in its own pull request (`CLAUDE.md`, `/implement-ticket`), and review fixes keep it in step with the code. Copy [`tickets/_template.md`](tickets/_template.md), and add the ticket to [`tickets/README.md`](tickets/README.md). If the ticket changed how a request flows, update the matching step in `overview.md`.

A page describes the code on `main`. If the code and a page disagree, the code is right, and the page needs fixing.

## Private notes

Personal notes go in `docs/notes/`, which git ignores. Nothing there is pushed, and nothing there is backed up by the repository either. The repository is public, so anything you don't want published belongs there, not in a tracked folder.
