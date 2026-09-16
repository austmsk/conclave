// Package agent implements the agent loop and the tools an implementer can
// call inside a sandbox: read_file, list_dir, search, write_file,
// apply_patch, run_tests, run_build, run_lint, run_command, and finish.
//
// Every path a tool touches is validated through a workspace root handle;
// invalid paths are rejected, never sanitized.
package agent
