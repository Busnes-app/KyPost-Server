#!/usr/bin/env bash
# Fail if a tracked file is a compiled executable (OpenSSF Scorecard Binary-Artifacts).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

types='application/(x-(executable|pie-executable|sharedlib|object|archive|mach-binary|dosexec)|vnd\.microsoft\.portable-executable|java-archive|x-java-applet|wasm)$'
# Add a pathspec exclusion such as ':!gradle/wrapper/gradle-wrapper.jar' only for a
# binary the repo must carry, with the reason beside it.
found=$(git ls-files -z -- . | sed -z 's|^|./|' | xargs -0 -r file --mime-type -- | grep -E "$types" || true)
if [ -n "$found" ]; then
  echo "Compiled binaries are tracked; build them from source instead:" >&2
  echo "$found" >&2
  exit 1
fi
