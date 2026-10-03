#!/usr/bin/env bash
# Engram — SessionEnd hook for Claude Code
# Go owns project/runtime-directory validation and non-creating closure.
# Keep host shutdown silent and fail-open, including with an older binary.

INPUT=$(cat)
printf '%s' "$INPUT" | engram hook claude-session-end > /dev/null 2>&1 || true
exit 0
