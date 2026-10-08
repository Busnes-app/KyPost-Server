#!/bin/sh
set -eu

# Docker applies KYPOST_BIND on the host side, so validate the published
# address before starting the cleartext listener. A remote proxy must either
# encrypt this hop or explicitly acknowledge the risk.
#
# Unset falls through to the refusal on purpose. This cannot see the real
# publish address (that is `-p`, on the host), so it is an acknowledgement gate,
# not a boundary — and an operator who runs the image outside compose and never
# says how it is reached is exactly who the gate is for. Do not add an empty
# arm here to make a bare `docker run` quieter; teach the caller to pass
# KYPOST_BIND, the way .github/workflows/ci.yml does.
case "${KYPOST_BIND:-}" in
	127.*|::1|\[::1\]|localhost) ;;
	*)
		if [ -z "${TLS_CERT_FILE:-}" ] && [ -z "${TLS_KEY_FILE:-}" ] && [ "${ALLOW_INSECURE_HTTP:-}" != "true" ]; then
			echo "refusing non-loopback cleartext HTTP; configure TLS_CERT_FILE/TLS_KEY_FILE or set ALLOW_INSECURE_HTTP=true" >&2
			exit 1
		fi
		;;
esac

# All four data dirs, plus the model cache. The image creates these, but a
# volume or bind mount can be mounted over any of them, and `set -e` means a
# chown against a missing path below would abort the boot.
mkdir -p /kypost/config /kypost/private /kypost/logs /kypost/state /kypost/ollama-models
# Bind mounts replace image permissions; protect authority, keys and mail
# before bootstrap writes credentials or unprivileged services start.
# CAP_CHOWN lets root take ownership before chmod without CAP_FOWNER.
# Change only the roots here; the existing handoff below restores runtime ownership.
chown root:root /kypost/config /kypost/private /kypost/state
chmod 0700 /kypost/config /kypost/private /kypost/state

# Runs synchronously (as root, before the chown below) so admin.env exists
# before any service starts — a hard guarantee that supervisord's
# priority-based program ordering could only approximate.
/bin/sh /opt/kypost/scripts/bootstrap.sh

# Mounted volumes arrive owned by whoever created them on the host, so this
# has to run as root. It is the only reason this script starts privileged,
# and it is why the image has no `USER kypost` line.
#
# Scoped to the four data volumes, NOT to /kypost as a whole. /kypost also
# contains ollama-models, which docker-compose.yml maps to a host bind mount
# (OLLAMA_MODELS_HOST_DIR, ./share/ollama/models by default) holding tens of
# gigabytes of model blobs. `chown -R /kypost` walked all of it on every single
# container start — minutes of I/O before the API could bind, repeated on every
# restart, and it rewrote ownership on the host's own files, breaking any
# ollama running there under a different UID.
chown -R kypost:kypost /kypost/config /kypost/private /kypost/logs /kypost/state

# The models directory only needs to be traversable and writable at the top
# level for Ollama to pull into it; a non-recursive chown is O(1) regardless of
# how much is already cached there. If the host bind mount is read-only or
# owned by another user this is allowed to fail: a pre-populated, read-only
# model cache is a legitimate setup, and Ollama only needs to read it.
chown kypost:kypost /kypost/ollama-models 2>/dev/null \
	|| echo "note: could not chown /kypost/ollama-models; continuing (read-only or externally owned mount)"

# Convert native mail storage to the current format before any service reads
# it, as the runtime user so every file it creates is owned by that user.
# Idempotent, and a no-op without native files. A failure must not stop the
# container: the services refuse half-migrated native state on their own while
# external IMAP users keep working.
setpriv --reuid=kypost --regid=kypost --init-groups \
	/usr/local/bin/kypost-server migrate-native \
	|| echo "native mail storage migration failed; what the error above names (all native mail, or only the mailboxes it lists) stays refused. Fix it and restart, or restore the pre-migration backup (docs/RESTORE.md)" >&2

# Drop to the unprivileged user for everything from here on, explicitly,
# rather than relying on supervisord's own `user=` option to do it. Two
# reasons: PID 1 itself is then unprivileged (so a container escape does not
# start from root), and the drop no longer depends on a setting in a config
# file that someone could edit without realizing it was load-bearing.
exec setpriv --reuid=kypost --regid=kypost --init-groups \
	supervisord -c /etc/supervisord.conf
