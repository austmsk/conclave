# 0011 — Sandboxes are released by ID, cancellation is a reason, and the trial image is built locally

**Date:** 2026-10-26
**Status:** proposed

## Context

Ticket 8's workflow has to release its sandbox on every exit path, including cancellation and a worker restart (NFR-R3, RL-3). The schema was built for that: `attempts.sandbox_id`, `attempts.sandbox_released_at`, and a partial index over the unreleased rows. But nothing wrote either column, and nothing could remove a sandbox without the live `contracts.Sandbox` object. A worker that died mid-attempt would leave its deferred cleanup unable to name the container. "Zero orphaned sandboxes", the Milestone 1 criterion, could only be checked with `docker ps`, not from the database.

The same review turned up two more gaps:
- **Cancellation had no honest record.** Attempt states are `running`, `passed`, `failed` and `errored`, and no `FailureReason` described an owner's `/cancel`. A cap refusal had the same problem: the only candidate was `provider_error`, which is false.
- **No configuration held the sandbox image.** `docker.Provider.Create` refuses an image that isn't pinned by digest (FR-SEC30). The java21 image existed only on the owner's machine, and FR-SEC30's CI build and GHCR publication have no Milestone 1 ticket.

## Decision

- **`SandboxProvider.Release(ctx, id)` joins the contract.**
  - It is idempotent: an ID that names nothing returns nil.
  - It refuses anything without the `conclave.sandbox` label.
  - The Docker version also removes the sandbox's network, which it finds by name because the install phase has already disconnected it.
  - It ignores the provider's namespace, because the ID comes from an attempt row another worker may have written.
- **The sandbox ID is recorded straight after creation, not before.** The Docker provider names its own containers. Recording the ID first would mean the provider accepting a name chosen by the caller, which is a wider change than Milestone 1 needs. `store.SetAttemptSandbox` is ticket 8's first database write after `Create`, and it is write-once. A crash between the two is covered by `Reap`'s sweep by age. `store.MarkSandboxReleased` follows a successful `Release`, and `store.UnreleasedAttempts` is the reaper's query.
- **`CreateWithInstall` moves onto the contract**, so the install phase is configuration (`deploy/sandboxes.yaml`) and Milestone 2's SWE-bench images need no new code. The contract's comment says "returns an error" rather than naming `docker.ErrInstallFailed`, because contracts can't depend on a backend.
- **Cancellation is a reason, not a state.** An attempt cancelled mid-run is `errored` with `ReasonCancelled`, and a cap refusal is `errored` with `ReasonBudgetCap`. A fifth state would have meant changing the transition table, its exhaustive test, the schema CHECK and the state-transition trigger, and the attempt's lifecycle doesn't differ: it stops and isn't counted as an agent failure.
- **The Milestone 1 image is built locally.** `deploy/local/sandboxes.yaml` names the local java21 image by digest. The CI build, weekly rebuild and GHCR publication from FR-SEC30 are deferred to ticket 10 or later.

## Consequences

- Ticket 8's deferred cleanup needs only the recorded ID. It calls `Release` from a disconnected context, then `MarkSandboxReleased`, and does nothing if no ID was recorded.
- A sandbox lost between `Create` and `SetAttemptSandbox` is invisible to the database. Only the Docker label and `Reap` find it. Ticket 10's orphan check therefore uses both `UnreleasedAttempts` and `docker ps` filtered by the label, as `m1-exit-checklist.md` already says.
- If the container is removed and the network removal then fails, a retry returns nil and the network waits for `Reap`.
- Resolve-rate queries must count `errored` attempts with `cancelled` or `budget_cap` as neither passes nor agent failures.
- Until FR-SEC30's pipeline exists, the image's provenance is the owner's machine. The digest pins what runs, but not how it was built.

## Amendment, 2026-11-21: the image build moves to the start of Milestone 2, with a backup until then

"Deferred to ticket 10 or later" did not happen. Milestone 1 closed with the java21 sandbox image held in one place only, the owner's Docker daemon:

- **The recipe is in the repository:** `deploy/images/java21/Dockerfile`, with its base image pinned by digest. Its `apt-get install git` is not pinned, so a rebuild can produce a different image.
- **The pin is not:** `deploy/local/sandboxes.yaml` is git-ignored, and names `localhost:5055/conclave-sandbox-java21@sha256:8fcd9711…`.
- **The registry that name points to is not running.** The image works only because `ensureImage` pulls only when the daemon lacks the image. The ticket 10b trial log recorded this on 2026-11-16.

A `docker system prune -a`, a Docker Desktop reset or a new machine would therefore leave no sandbox image. The worker would start normally and every attempt would fail at sandbox creation, trying to pull from a registry that isn't there.

Two changes follow:

1. **Until the pipeline exists, the owner keeps a `docker save` archive of each pinned image outside the repository and off the daemon.** The daemon uses the containerd image store, so the archive holds the original manifest, and `docker load` restores the same digest the pin names. The restore steps are in `docs/runbooks.md`, "Sandbox image lost". This protects against losing the image, not against losing the machine, and it gives no provenance.
2. **FR-SEC30's image build is the first Milestone 2 ticket that touches images, before any new image is made.** Milestone 2's first vertical slice already needs a Python image, and the plan adds five languages and 25 SWE-bench instances. Building each by hand would multiply this gap. The ticket:
   - builds each `deploy/images/<name>` in GitHub Actions when its directory changes, and weekly;
   - pushes it to GitHub Container Registry as a private package and records the digest it produced;
   - scans the image, and opens a pull request that changes the digest in the shipped `deploy/sandboxes.yaml`, so every digest change is reviewed (threat I11);
   - pins the packages each Dockerfile installs, so a rebuild without an intended change produces the same contents.

   The worker host then pulls by digest from GHCR with a read-only package token, and the local registry name goes away.

The same ticket should add a startup check to the worker: confirm every image named in `sandboxes.yaml` is present or pullable, and refuse to start otherwise. A lost image then stops the worker once, with a message naming the image, instead of failing every attempt.

Until this lands, the image's provenance is still the owner's machine.

