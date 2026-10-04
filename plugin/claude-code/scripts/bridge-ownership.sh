#!/usr/bin/env bash
# Scoped transport claims only; native Engram owns proof of successful writes.
# Do not use these markers to suppress an entire hook or authorize core writes.
ENGRAM_BRIDGE_JS_TRIM='def js_trim: gsub("^[\u0009-\u000d\u0020\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff]+|[\u0009-\u000d\u0020\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff]+$"; "");'

engram_bridge_owns() (
  # Subshell isolates pipefail and locals from the calling hook.
  set -o pipefail
  local kind="$1" marker digest hash
  local binary_output=()
  # Native Windows jq otherwise converts embedded LF to CRLF on stdout.
  case "${OSTYPE:-}" in msys*|cygwin*|win32*) binary_output=(-b) ;; esac
  command -v jq >/dev/null 2>&1 || return 1
  case "$kind" in
    registration) marker="${PI_ENGRAM_BRIDGE_SESSION_REGISTER:-}" ;;
    capture) marker="${PI_ENGRAM_BRIDGE_PROMPT_CAPTURE:-}" ;;
    *) return 1 ;;
  esac
  [ -n "$marker" ] || return 1
  printf '%s' "$INPUT" | jq -se --arg marker "$marker" "$ENGRAM_BRIDGE_JS_TRIM"'
    ($marker | fromjson) as $m |
    length == 1 and (.[0] | type) == "object" and
    ($m | type) == "object" and $m.version == 1 and
    ($m.runtimeSessionId | type) == "string" and
    ($m.runtimeSessionId | js_trim | length) > 0 and
    ($m.claudeSessionId | type) == "string" and
    ($m.claudeSessionId | js_trim | length) > 0 and
    (.[0].session_id | type) == "string" and
    .[0].session_id == $m.claudeSessionId
  ' >/dev/null 2>&1 || return 1
  [ "$kind" = registration ] && return 0
  digest=$(printf '%s' "$marker" | jq -er '.promptDigest | select(type == "string") | select(length == 64 and test("^[0-9a-f]{64}$"))') || return 1
  command -v sha256sum >/dev/null 2>&1 || return 1
  # Stream decoded original UTF-8 directly: Bash cannot retain NUL or trailing LF.
  # Slurp validates the complete payload before any prompt bytes are emitted.
  hash=$(printf '%s' "$INPUT" | jq "${binary_output[@]}" -sj "$ENGRAM_BRIDGE_JS_TRIM"'
    if length == 1 and (.[0].prompt | type) == "string"
    then .[0].prompt | js_trim else error("invalid prompt") end
  ' | sha256sum) || return 1
  hash="${hash%% *}"
  [[ "$hash" =~ ^[0-9a-f]{64}$ ]] && [ "$hash" = "$digest" ]
) 2>/dev/null
