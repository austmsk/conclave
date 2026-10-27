package store

import "strconv"

// Actor strings name who caused a state change. Transition sets the
// string on the transaction as conclave.actor, and the
// record_state_transition trigger copies it into state_transitions.actor
// (FR-SEC10). The column is read by people deciding what happened, not
// parsed by code, so the prefix is the only structure: "workflow:" for a
// change the platform made while running a workflow, "github:" for an
// owner command, and the bare ActorWorker for jobs that belong to no
// workflow. The string is self-reported; the authenticated login is
// recorded beside it as db_user.

// ActorWorker attributes changes made by the worker outside any workflow:
// seeding repositories at startup and the orphan reaper.
const ActorWorker = "worker"

// ActorWorkflow attributes a platform transition to the workflow that made
// it, by its Temporal workflow ID (RL-14).
func ActorWorkflow(workflowID string) string {
	return "workflow:" + workflowID
}

// ActorGitHub attributes a transition to the GitHub user whose command
// caused it, by numeric user ID, which unlike a login cannot be renamed.
func ActorGitHub(userID int64) string {
	return "github:" + strconv.FormatInt(userID, 10)
}
