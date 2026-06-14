#!/bin/bash
# PostToolUse hook: gofmt any Go file Claude edits or writes.
# Receives the tool-call JSON on stdin; exits 0 always (never block the edit).
f="$(jq -r '.tool_input.file_path // empty' 2>/dev/null)"
if [[ "$f" == *.go && -f "$f" ]]; then
  gofmt -w "$f" 2>/dev/null
fi
exit 0
