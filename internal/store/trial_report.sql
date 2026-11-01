-- Trial report: one row per trial over a window of time (ticket 10a).
--
-- A trial is one label on an issue, which is one feature request: a /retry
-- continues the same feature request and is not a new trial. This is the
-- one query the Milestone 1 trial report is produced from, and the one
-- Milestone 2's report generator starts from (resolve rate, cost per
-- resolved task, time per task; FR-A4, NFR-P6). store.ReadTrialReport runs
-- it and computes the summary from its rows.
--
-- Parameters:
--   $1  timestamptz  start of the window, inclusive
--   $2  timestamptz  end of the window, exclusive
-- A feature request is in the window when its created_at is, that is, when
-- the owner's label reached the platform.
--
-- To run it by hand in psql 16 or later: the file ends without a
-- semicolon, so \i leaves the query in the buffer, and \bind runs it:
--   \i internal/store/trial_report.sql
--   \bind '2026-11-01T00:00:00Z' '2026-12-01T00:00:00Z' \g
-- Or use `conclave report -from ... -to ...`, which runs this same file.
--
-- Columns:
--   feature_request_id    the trial's feature request.
--   repo                  repos.full_name.
--   issue                 the issue number that was labelled.
--   state                 the feature request's state now. delivered means
--                         the pull request's CI passed (decision 0014).
--   attempts              every attempt of every task of every plan of the
--                         feature request, all ordinals, all outcomes.
--   input_tokens,
--   output_tokens,
--   cost_usd              summed from cost_events by feature_request_id, so
--                         every role's spend counts, the pr_writer's too,
--                         and a call outside any attempt.
--   agent_seconds         the sum of finished_at - started_at over finished
--                         attempts: time the agent and the gates held a
--                         sandbox. A running attempt adds nothing.
--   approve_to_delivered_seconds
--                         from the plan's approval to the feature request's
--                         move to delivered, from state_transitions: the
--                         time per task the owner waits after /approve. Zero
--                         when the feature request is not delivered. Time
--                         spent waiting in needs_attention for /retry counts,
--                         as it does for the owner.
--
-- Every subquery is correlated on the feature request, so the per-row sums
-- cannot multiply each other as joins would.

SELECT
    f.id::text                                   AS feature_request_id,
    r.full_name                                  AS repo,
    f.issue_number                               AS issue,
    f.state                                      AS state,
    (SELECT count(*)
       FROM attempts a
       JOIN tasks t ON t.id = a.task_id
       JOIN plans p ON p.id = t.plan_id
      WHERE p.feature_request_id = f.id)         AS attempts,
    (SELECT coalesce(sum(c.input_tokens), 0)
       FROM cost_events c
      WHERE c.feature_request_id = f.id)::bigint AS input_tokens,
    (SELECT coalesce(sum(c.output_tokens), 0)
       FROM cost_events c
      WHERE c.feature_request_id = f.id)::bigint AS output_tokens,
    (SELECT coalesce(sum(c.cost_usd), 0)
       FROM cost_events c
      WHERE c.feature_request_id = f.id)::float8 AS cost_usd,
    (SELECT coalesce(sum(extract(epoch FROM a.finished_at - a.started_at)), 0)
       FROM attempts a
       JOIN tasks t ON t.id = a.task_id
       JOIN plans p ON p.id = t.plan_id
      WHERE p.feature_request_id = f.id
        AND a.finished_at IS NOT NULL)::float8   AS agent_seconds,
    coalesce(extract(epoch FROM
        (SELECT max(s.occurred_at)
           FROM state_transitions s
          WHERE s.entity_kind = 'feature_request'
            AND s.entity_id = f.id
            AND s.to_state = 'delivered')
      - (SELECT max(p.approved_at)
           FROM plans p
          WHERE p.feature_request_id = f.id)
    ), 0)::float8                                AS approve_to_delivered_seconds
FROM feature_requests f
JOIN repos r ON r.id = f.repo_id
WHERE f.created_at >= $1
  AND f.created_at < $2
ORDER BY f.created_at, f.id
