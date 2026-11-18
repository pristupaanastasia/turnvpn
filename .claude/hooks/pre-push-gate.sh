#!/usr/bin/env bash
# PreToolUse hook on Bash. For `git push` commands it pauses the tool call and
# reminds Claude to run the security-audit skill (and the pre-push-reviewer
# subagent) before pushing. Any other command passes through untouched.
set -euo pipefail

input="$(cat)"
cmd="$(printf '%s' "$input" | jq -r '.tool_input.command // ""')"

# Match a real `git push` (not e.g. a commit message mentioning push).
if printf '%s' "$cmd" | grep -Eq '(^|[;&| ])git([[:space:]]+-[^[:space:]]+)*[[:space:]]+push($|[[:space:]])'; then
  jq -nc '{
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "ask",
      permissionDecisionReason: "Before this push: (1) run the /security-audit skill — review the app, server, and connection for secret/token leaks, open-proxy and SSRF; (2) run the pre-push-reviewer subagent on upstream..HEAD and confirm VERDICT: PASS. Approve only once the security audit has no critical findings and review has passed."
    }
  }'
  exit 0
fi

# Not a push: allow silently.
exit 0
