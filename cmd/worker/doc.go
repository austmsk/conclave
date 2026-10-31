// Command worker runs Conclave's Temporal workers: one polls the feature
// task queue for FeatureWorkflow, TaskWorkflow and the small activities,
// and one polls the attempt queue with a single slot, so attempts run one
// at a time.
//
// At startup it loads and validates every deploy file (-router, -repos,
// -toolchains, -sandboxes, -gates, -github-config; the shipped deploy/
// files carry placeholders that refuse to load, so the owner points the
// flags at deploy/local/), checks the four per-repository files name the
// same repositories, records each file's SHA-256 for the plan bodies,
// seeds the repos table from configuration, and sweeps sandboxes an
// earlier worker left unreleased (the reaper half of NFR-R3). Secrets come
// from the environment and are unset once read: CONCLAVE_DATABASE_URL,
// CONCLAVE_ANTHROPIC_API_KEY and CONCLAVE_GITHUB_APP_KEY_FILE.
package main
