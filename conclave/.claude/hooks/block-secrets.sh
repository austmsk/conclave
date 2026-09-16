#!/usr/bin/env bash
# Blocks tool calls that reference secret material.
#
# Deny rules in settings.json are the first layer, but they have been reported
# not to catch every path to a file, and a shell command reaches a file by a
# different route than the Read tool. This hook is the second layer.
#
# The real defence is that secrets live outside the project directory entirely
# (~/.config/conclave/secrets.env). This catches mistakes, not attacks.
#
# Exit 2 blocks the tool call and returns stderr to the model.

set -euo pipefail

payload=$(cat)

patterns=(
  '\.env($|[^.a-zA-Z])'
  '\.config/conclave'
  '\.ssh/'
  '\.aws/credentials'
  'sk-ant-'
  'github_pat_'
  'ghs_'
  'BEGIN [A-Z ]*PRIVATE KEY'
)

for p in "${patterns[@]}"; do
  if grep -Eq "$p" <<<"$payload"; then
    echo "Blocked: this tool call references secret material ($p)." >&2
    echo "Secrets live in ~/.config/conclave/secrets.env, outside the repo." >&2
    echo "Use canary values (canary_*) in tests and fixtures instead." >&2
    exit 2
  fi
done

exit 0
