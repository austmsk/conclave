// Package workflow implements Conclave's Temporal workflows: the feature
// workflow (analyze, implement, gate, deliver), its task children, and the
// per-repository integration workflow.
//
// Code here is deterministic: no time.Now, no math/rand, no raw goroutines,
// no network calls, and no direct database access. Use workflow.Now,
// workflow.Go, and workflow.NewSelector instead. Maps are never ranged over
// without sorting keys first, since Go randomizes map iteration order and
// replay would diverge. Every side effect belongs in internal/activity.
package workflow
