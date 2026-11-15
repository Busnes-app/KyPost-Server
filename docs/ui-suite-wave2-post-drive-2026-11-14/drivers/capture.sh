#!/bin/bash
# Builds each branch and runs the w2 capture scenarios before (origin/main frontend) and after.
set -u
export PATH=$HOME/.local/share/ky-toolchains/node-v26.13.0-linux-x64/bin:$PATH
W=$HOME/git/KyPost-Server/.claude/worktrees; T=/tmp/w2-post-drive; FIX=$HOME/git/kypost-demo-IMAP
build() { # worktree -> $T/bin-<wt>, $T/dist-<wt>
  (cd $W/$1/frontend && npx vite build --outDir $T/dist-$1 >/dev/null 2>&1) && (cd $W/$1/backend && CGO_ENABLED=0 go build -trimpath -o $T/bin-$1 ./cmd/) && echo "built $1" || echo "BUILD FAILED $1"
}
for wt in "$@"; do build $wt; done
run() { # bin dist out prefix scenario port
  timeout 1200 node $T/drivers/w2-captures.mjs $T/$1 $T/$2 $FIX $T/$3 $4 $5 $6 2>&1 | grep -E '^\{"(file|coop|tag)' || echo "SCENARIO FAILED $4"
}
for wt in "$@"; do
  case $wt in
    drive-picker) run bin-$wt dist-before shots-picker kypost-picker-before picker 5891; run bin-$wt dist-$wt shots-picker kypost-picker-after picker 5891 ;;
    compose-link) run bin-$wt dist-before shots-deeplink kypost-deeplink-before deeplink 5891; run bin-$wt dist-$wt shots-deeplink kypost-deeplink-after deeplink 5891 ;;
    guest-links) run bin-$wt dist-before shots-guest kypost-guest-before guest 5891; run bin-$wt dist-$wt shots-guest kypost-guest-after guest 5891 ;;
  esac
done
