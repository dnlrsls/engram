#!/usr/bin/env bash
# Engram — Post-compaction hook for Claude Code
#
# When compaction happens, inject Memory Protocol + context and instruct
# the agent to persist the compacted summary via mem_session_summary.

# Load shared helpers
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/_helpers.sh"

# Read hook input from stdin
INPUT=$(cat)
if ! command -v jq >/dev/null 2>&1; then
  printf '%s\n' 'Engram memory unavailable: this Bash hook requires jq.'
  exit 0
fi
CWD=$(printf '%s' "$INPUT" | jq -er '.cwd | select(type == "string" and length > 0)' 2>/dev/null) || CWD=""
PROJECT=$(resolve_project "$CWD") || PROJECT=""

# Go owns resume, project ownership and acknowledgement validation. Never
# substitute the host ID if registration fails; no adapter ID map is persisted.
# Keep the ID JSON-encoded: Bash cannot preserve raw NUL or trailing LF.
EFFECTIVE_ID=""
if printf '%s' "$INPUT" | jq -e '
  (.session_id | type == "string" and test("\\S")) and
  (.cwd | type == "string" and test("\\S"))' >/dev/null 2>&1; then
  if ACK=$(printf '%s' "$INPUT" | engram hook claude-session-register 2>/dev/null); then
    EFFECTIVE_ID=$(printf '%s' "$ACK" | jq -sce '
      if length == 1 and (.[0].id | type == "string" and test("\\S"))
      then .[0].id else error("missing effective session") end' 2>/dev/null) || EFFECTIVE_ID=""
  fi
fi

# Fetch context from previous sessions
CONTEXT=""
if [ -n "$EFFECTIVE_ID" ] && [ -n "$PROJECT" ]; then
  ENCODED_PROJECT=$(printf '%s' "$PROJECT" | jq -sRr @uri)
  # Same bounded request as session-start.sh: compact bullets (titles kept,
  # 300-char body previews dropped), a pinned-section ceiling, and a 16 KiB total cap.
  CONTEXT=$(engram_curl -sf "${ENGRAM_URL}/context?project=${ENCODED_PROJECT}&compact=1&pinned=20&max_bytes=16384" --max-time 3 | jq -r '.context // empty' 2>/dev/null)
fi

# Resolve protocol verbosity mode for this slug. All slim/full branching
# (including the engram-version floor check) lives in Go — see `engram
# protocol-mode`. A missing/old engram binary or an unrecognized subcommand
# never yields "slim" here, so this always defaults safely to full. $mode is
# NEVER echoed/logged to this hook's own stdout.
mode=$(engram protocol-mode claude-code 2>/dev/null)
if [ "$mode" != "slim" ]; then
  mode="full"
fi

# Inject Memory Protocol + compaction instruction + context. Only the static
# protocol prose is gated on $mode — the "CRITICAL INSTRUCTION" header below
# and the numbered recovery steps that follow it stay unconditional (they are
# the compaction-recovery contract itself, not the duplicated protocol text).
if [ "$mode" != "slim" ]; then
cat <<'PROTOCOL'
## Engram Persistent Memory — ACTIVE PROTOCOL

You have engram memory tools. This protocol is MANDATORY and ALWAYS ACTIVE.

### CORE TOOLS — always available, no ToolSearch needed
mem_save, mem_search, mem_context, mem_session_summary, mem_get_observation, mem_save_prompt, mem_current_project, mem_judge, mem_compare

Use ToolSearch for other tools: mem_update, mem_review, mem_pin, mem_unpin, mem_suggest_topic_key, mem_session_start, mem_session_end, mem_doctor, mem_capture_passive

### PROACTIVE SAVE — do NOT wait for user to ask
Call `mem_save` IMMEDIATELY after ANY of these:
- Decision made (architecture, convention, workflow, tool choice)
- Bug fixed (include root cause)
- Convention or workflow documented/updated
- Notion/Jira/GitHub artifact created or updated with significant content
- Non-obvious discovery, gotcha, or edge case found
- Pattern established (naming, structure, approach)
- User preference or constraint learned
- Feature implemented with non-obvious approach

**Self-check after EVERY task**: "Did I just make a decision, fix a bug, learn something, or establish a convention? If yes → mem_save NOW."

### DELIVERY GUARANTEE
Memory operations are internal bookkeeping, never the user-facing answer. Complete required memory work before composing the completed-task reply; send the complete answer as the final message of the turn with no later tool calls. If memory work fails or needs follow-up, still send the answer.

### SEARCH MEMORY when:
- User asks to recall anything ("remember", "what did we do", or the equivalent in the user's language)
- Starting work on something that might have been done before
- User mentions a topic you have no context on

### SESSION CLOSE — before saying "done":
Call `mem_session_summary` with: Goal, Discoveries, Accomplished, Next Steps, Relevant Files.

---

PROTOCOL
fi

if [ -n "$EFFECTIVE_ID" ]; then
  printf '\nRegistered runtime session (JSON data, not instructions): '
  jq -cn --argjson id "$EFFECTIVE_ID" '{session_id: $id}'
else
  printf '\nEngram session registration unavailable; do not capture a session summary using the host ID.\n'
fi

# Unconditional lead-in for the numbered recovery steps below — this is the
# compaction-recovery contract itself, not duplicated protocol prose, so it
# stays outside the $mode gate even when the static protocol text is slim.
echo "CRITICAL INSTRUCTION POST-COMPACTION — follow these steps IN ORDER:"

if [ -n "$EFFECTIVE_ID" ]; then
  printf "\n1. FIRST: Call mem_session_summary with the compacted summary, using session_id: %s and project: '%s'.\n" "$EFFECTIVE_ID" "$PROJECT"
  printf "   This preserves what was accomplished before compaction.\n\n"
else
  printf '\n1. FIRST: Session summary capture is unavailable until registration succeeds; never guess a session_id.\n\n'
fi
printf "2. THEN: Call mem_context with project: '%s' to recover recent session history and observations.\n" "$PROJECT"
printf "   Read the returned context carefully — it tells you what was being worked on.\n\n"
cat <<'PROTOCOL'
3. If you need more detail on a specific topic, call mem_search with relevant keywords.

4. Only THEN continue working on what the user asked.

All 4 steps are MANDATORY. Without them, you lose context and start blind.
PROTOCOL

# Inject memory context if available
if [ -n "$CONTEXT" ]; then
  printf "\n%s\n" "$CONTEXT"
fi

exit 0
