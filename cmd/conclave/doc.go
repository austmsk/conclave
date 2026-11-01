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
//     stood (unless -keep), and unpauses the reaper.
//   - alert-test: raises a test alert, to prove the local log and issue
//     delivery work (FR-SEC31).
//   - report: prints the trial report from the database: resolve rate,
//     cost per resolved task and time per task.
//
// Secrets come from the environment, as for the worker, and are unset
// once read.
package main
