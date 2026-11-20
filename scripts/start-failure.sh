#!/bin/sh
# supervisord event listener: show why the API or daemon will not start where an operator
# looks. A program that exits before startsecs (a startup refusal: settings that differ from
# the deployment's, volumes from another deployment, a replica on another host) enters
# BACKOFF. This copies the last lines of its error log to the container's stderr, so
# `docker compose logs` names the reason instead of only "exited: api".
#
# Reads the supervisor event protocol on stdin and answers on stdout, so it prints nothing
# else to stdout. KYPOST_LOG_DIR is for the self-check only.
set -eu

logs=${KYPOST_LOG_DIR:-/kypost/logs}
printf 'READY\n'

while IFS= read -r header; do
	length=$(printf '%s' "$header" | tr ' ' '\n' | sed -n 's/^len://p')
	payload=''
	if [ -n "${length:-}" ] && [ "$length" -gt 0 ] 2>/dev/null; then
		payload=$(dd bs=1 count="$length" 2>/dev/null)
	fi
	name=$(printf '%s' "$payload" | tr ' ' '\n' | sed -n 's/^processname://p')
	case "$name" in
		api | daemon)
			printf 'kypost %s did not start; the end of %s/%s.err.log:\n' "$name" "$logs" "$name" >&2
			tail -n 5 "$logs/$name.err.log" >&2 2>/dev/null || true
			;;
	esac
	printf 'RESULT 2\nOK'
	printf 'READY\n'
done
