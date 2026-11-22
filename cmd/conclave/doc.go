// Command conclave is the owner's command line on the central machine.
// Its single responsibility is the operations that must not depend on
// GitHub or on a running worker:
//
//   - halt: the kill switch (FR-SEC11). It records the halt in Postgres,
//     raises the halt alert, pauses the scheduled reaper, cancels every
//     open workflow, sweeps every sandbox with no age cutoff, and reports
//     what remains. Workers see the halt, stop after their grace period,
//     and refuse to start while it stands.
//   - resume: clears the halt, after cancelling every run started while it
//     stood (unless -keep), unpauses the reaper, and raises a resume alert
//     on the halt's issue. It runs only with a terminal on stdin and only
//     once the owner types the halt's ID, so a script or an agent cannot
//     lift a halt.
//   - alert-test: raises a test alert, to prove the local log and issue
//     delivery work (FR-SEC31).
//   - report: prints the trial report from the database: resolve rate,
//     cost per resolved task and time per task.
//
// Secrets come from the environment and are unset once read. The
// database URL is CONCLAVE_OPERATOR_DATABASE_URL, a login in
// conclave_operator, which alone may record a halt or a resume; it is
// never the worker's login (decision 0002). halt warns about a wrong login
// and tries anyway; every other command refuses one.
package main
