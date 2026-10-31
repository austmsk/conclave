-- Command audit: one row for every owner command that reaches a workflow,
-- accepted or rejected, with its actor and time (FR-SEC10).
--
-- The workflow decides a command against the status it holds at that
-- moment and writes the row from an activity. The comment ID names the
-- command: a redelivered or replayed webhook carries the same ID, so a
-- retried write, or a second delivery of one command, is absorbed by the
-- unique key rather than doubled. The table is append-only: the
-- application role may read and insert, never update or delete (threat
-- T6). Commands from anyone but the owner, and malformed ones, never reach
-- a workflow and stay in the receiver's log, as in ticket 6.

-- +goose Up

CREATE TABLE commands (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    comment_id         bigint      NOT NULL UNIQUE CHECK (comment_id > 0),
    delivery_id        text        NOT NULL,
    -- The workflow that decided the command: the issue's feature workflow,
    -- or the closed-run workflow that answered it after the run ended.
    workflow_id        text        NOT NULL CHECK (workflow_id <> ''),
    repo_github_id     bigint      NOT NULL CHECK (repo_github_id > 0),
    issue_number       integer     NOT NULL CHECK (issue_number > 0),
    -- Null when the command arrived before analysis recorded the feature
    -- request, or for an issue that has none.
    feature_request_id uuid        REFERENCES feature_requests (id),
    command            text        NOT NULL CHECK (command IN (
                           'approve', 'revise', 'ask', 'cancel', 'status', 'retry', 'help')),
    -- store.ActorGitHub: github:<numeric user ID>.
    actor              text        NOT NULL CHECK (actor LIKE 'github:%'),
    -- The feature request's status when the command was decided. Null
    -- when there is no feature request.
    status_at_decision text        CHECK (status_at_decision IN (
                           'planning', 'awaiting_approval', 'executing',
                           'needs_attention', 'delivered', 'cancelled')),
    outcome            text        NOT NULL CHECK (outcome IN ('accepted', 'rejected')),
    -- Why a command was rejected: not_valid_in_status, not_available,
    -- over_cap:<kind>, closed_run, no_run, no_workflow, and the like.
    reason             text,
    recorded_at        timestamptz NOT NULL DEFAULT now(),
    CHECK ((outcome = 'rejected') = (reason IS NOT NULL))
);

CREATE INDEX commands_feature_request ON commands (feature_request_id, recorded_at);

-- Append-only, granted explicitly: 00004 sets no default privileges.
GRANT SELECT, INSERT ON commands TO conclave_app;

-- +goose Down

REVOKE ALL PRIVILEGES ON commands FROM conclave_app;
DROP TABLE commands;
