#!/bin/bash
export PATH=$HOME/.local/share/ky-toolchains/node-v26.13.0-linux-x64/bin:$PATH
T=/tmp/w2-post-drive; W=$HOME/git/KyPost-Server/.claude/worktrees/guest-links
(cd $W/frontend && npx vite build --outDir $T/dist-guest-links >/dev/null 2>&1) && (cd $W/backend && CGO_ENABLED=0 go build -trimpath -o $T/bin-guest-links ./cmd/) && echo built
rm -f $T/shots-guest/kypost-guest-after-*
timeout 1200 node $T/drivers/w2-captures.mjs $T/bin-guest-links $T/dist-guest-links $HOME/git/kypost-demo-IMAP $T/shots-guest kypost-guest-after guest 5891 2>&1 | grep -v '"level"' | tail -40
