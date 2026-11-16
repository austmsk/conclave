# Running Conclave locally

How to bring up a working Conclave on one machine: the dev stack, the worker, the webhook server and the tunnel GitHub delivers to. Run each long-lived process in its own terminal.

```
GitHub ──webhook──▶ cloudflared tunnel ──▶ server (127.0.0.1:8080) ──start workflow──▶ Temporal (7233)
                                                                                          │
                                         worker ◀──────────── polls task queues ──────────┘
                                           │  Postgres (5432), Docker sandboxes, GitHub App, model provider
```

The server only verifies webhooks and asks Temporal to start workflows. It holds no database connection and never loads the App's private key. Everything else, sandboxes, GitHub writes, model calls and Postgres, happens in the worker.

## 1. Prerequisites

- Go (the toolchain pinned in `go.mod`), Docker, and `cloudflared` (`brew install cloudflared`).
- The GitHub App, installed on the run repositories and on the private alerts repository.
- Owner configuration in `deploy/local/`, which git ignores. The shipped files in `deploy/` are placeholders that fail validation on purpose, so neither process starts against them. You need:

  | File | Used by | Holds |
  |---|---|---|
  | `github.yaml` | server, worker, `conclave` | App and installation IDs, owner and bot users, trigger label, alerts repository |
  | `repos.yaml` | server, worker | the enabled repositories, their sensitivity tier and roles |
  | `router.yaml` | worker | models, prices, output and spend caps, rate limits |
  | `toolchains.yaml` | worker | build, lint and test commands, repository profiles |
  | `sandboxes.yaml` | worker | sandbox image by digest, limits, install phase |
  | `gates.yaml` | worker | diff caps, protected and execution-surface additions, CI check |

  Each shipped `deploy/*.yaml` file's header comment says what its fields mean. `repos.yaml`, `toolchains.yaml`, `sandboxes.yaml` and `gates.yaml` must name the same repositories, and the alerts repository in `github.yaml` must not be one of them; the worker refuses to start otherwise.

- The sandbox image. Build `deploy/images/java21` (or the image your repository needs), push it to a registry the worker can pull from, and pin it in `deploy/local/sandboxes.yaml` as `name@sha256:<digest>`. A tag is refused.

## 2. Secrets

Secrets come from the environment, set from your secrets file outside the repository. Load it into the shell that starts each process, for example with `set -a; . <your secrets file>; set +a`. Each process reads its variables once at startup and unsets them, so nothing it starts later inherits them.

| Variable | Read by | What it is |
|---|---|---|
| `CONCLAVE_DATABASE_URL` | worker, `conclave` | Postgres URL for a login role in `conclave_app` (see step 3) |
| `CONCLAVE_ANTHROPIC_API_KEY` | worker | the model provider's API key |
| `CONCLAVE_GITHUB_APP_KEY_FILE` | worker, `conclave` | path to the App's private key file |
| `CONCLAVE_GITHUB_WEBHOOK_SECRET` | server | the App's webhook secret, at least 32 characters |

The worker stops at startup and names any variable that is missing. Never put a value in this repository, a flag, or a shell history line.

## 3. Start the dev stack

```
make dev       # Temporal (7233, UI on 8233), Postgres with pgvector (5432), Jaeger (UI on 16686)
make migrate   # apply migrations as the database owner
```

`make migrate` connects as the owner (`DATABASE_URL` in the `Makefile`). The worker must not: it connects as a login role that is a member of `conclave_app`, so the migrations' grants are the ceiling of what it can do (`internal/store/migrations/00004_app_role.sql`). Create that role once, outside the repository, choosing your own name and password:

```sql
CREATE ROLE <login> LOGIN PASSWORD '<password>' IN ROLE conclave_app;
```

Then set `CONCLAVE_DATABASE_URL` in your secrets file to `postgres://<login>:<password>@localhost:5432/conclave?sslmode=disable`.

## 4. Start the worker

In its own terminal, with the secrets loaded:

```
go run ./cmd/worker \
  -router deploy/local/router.yaml \
  -repos deploy/local/repos.yaml \
  -toolchains deploy/local/toolchains.yaml \
  -sandboxes deploy/local/sandboxes.yaml \
  -gates deploy/local/gates.yaml \
  -github-config deploy/local/github.yaml \
  -blob-dir <dir> \
  -work-dir <dir> \
  -alert-log <file>
```

`-blob-dir`, `-work-dir` and `-alert-log` are required and have no default. Keep them outside the repository and use the same paths on every start: the blob store holds attempt artifacts, the work directory holds clones, and the alert log is append-only and shared with `conclave halt`.

Other flags, with their defaults:

| Flag | Default | Meaning |
|---|---|---|
| `-gitleaks-config` | `deploy/gitleaks.toml` | secret-scan rules |
| `-temporal-address` | `localhost:7233` | Temporal frontend |
| `-temporal-namespace` | `default` | Temporal namespace |
| `-sweep-after` | `2h` | age past which a sweep releases an unreleased sandbox; must exceed the longest sandbox hold |
| `-reap-every` | `15m` | interval of the scheduled reaper |
| `-halt-grace` | `3m` | how long the worker keeps running after it sees a halt; must exceed the 2-minute heartbeat timeout |

On startup the worker:
1. loads and validates every configuration file, and stops on the first error;
2. refuses to start while a halt is in effect (`conclave resume` lifts it, see [`runbooks.md`](runbooks.md#halt));
3. records the enabled repositories, sweeps orphaned sandboxes and logs `startup sweep`;
4. creates or updates the `conclave-reaper` Temporal schedule, then polls its task queues.

**Check:** `temporal schedule list` shows `conclave-reaper`, and the Temporal UI at `localhost:8233` shows the worker polling.

## 5. Start the tunnel and point GitHub at it

Follow [`trials/webhook-tunnel.md`](trials/webhook-tunnel.md), steps 1 and 2: run `cloudflared tunnel --url http://127.0.0.1:8080` in its own terminal and set the App's Webhook URL to the printed address plus `/webhooks/github`. The address changes every time the tunnel restarts, so update the App each time.

## 6. Start the server

In its own terminal, with the secrets loaded:

```
go run ./cmd/server -github-config deploy/local/github.yaml -repos deploy/local/repos.yaml
```

Other flags: `-addr` (default `127.0.0.1:8080`, the address the tunnel forwards to), `-temporal-address` and `-temporal-namespace` (as for the worker).

**Check:** redeliver a recent delivery from the App's **Advanced → Recent Deliveries**. A `202` means GitHub, the tunnel and the server all work. The response codes for each failure are in [`trials/webhook-tunnel.md`](trials/webhook-tunnel.md#if-it-fails).

## 7. Run something

Add the trigger label from `github.yaml` to an issue on an enabled repository, then comment `/approve` on the plan the platform posts. Commands, statuses and what each step does are in [`guide/overview.md`](guide/overview.md). To stop everything in an emergency, use `conclave halt` ([`runbooks.md`](runbooks.md#halt)).

## Stopping

Stop in this order, each with Ctrl-C:

1. **Server**, so no new workflow starts.
2. **Worker.** An attempt in flight is not lost: Temporal times it out after 2 minutes without a heartbeat, and the next worker to start settles it, releases its sandbox and continues the workflow.
3. **Tunnel.**
4. `make down` stops the dev stack. Postgres and Temporal keep their data in Docker volumes, so open workflows resume when you start again.

## If it fails

| Symptom | Likely cause |
|---|---|
| `-blob-dir, -work-dir and -alert-log are required` | One of the three worker flags is missing. |
| `CONCLAVE_… is not set` | The secrets file was not loaded in that terminal, or names the variable differently. |
| An error naming a `deploy/local` file | That file is missing, still has placeholder values, or names a different set of repositories from the other per-repository files. |
| `alerts.repository … is also a run repository` | The alerts repository is listed in `repos.yaml`. Give alerts their own repository. |
| `halt <n> since …: halted by the kill switch` | A halt is in effect. Run `conclave resume` once you are sure it can be lifted. |
| `connecting to Temporal at …` | `make dev` is not running, or Temporal is still starting. |
| Webhook deliveries fail | See [`trials/webhook-tunnel.md`](trials/webhook-tunnel.md#if-it-fails). |
