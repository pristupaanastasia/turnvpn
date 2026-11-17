#!/usr/bin/env bash
# PreToolUse hook on Bash. For `git push` commands it pauses the tool call and
# reminds Claude to run the pre-push-reviewer subagent (and get VERDICT: PASS)
# before pushing. Any other command passes through untouched.
set -euo pipefail

input="$(cat)"
cmd="$(printf '%s' "$input" | jq -r '.tool_input.command // ""')"

# Match a real `git push` (not e.g. a commit message mentioning push).
if printf '%s' "$cmd" | grep -Eq '(^|[;&| ])git([[:space:]]+-[^[:space:]]+)*[[:space:]]+push($|[[:space:]])'; then
  jq -nc '{
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "ask",
      permissionDecisionReason: "Before this push: run the pre-push-reviewer subagent on upstream..HEAD and confirm VERDICT: PASS. Approve only once review has passed."
    }
  }'
  exit 0
fi

# Not a push: allow silently.
exit 0
