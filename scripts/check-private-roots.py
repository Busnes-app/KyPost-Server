#!/usr/bin/env python3
"""Check image and mounted-root permissions through the real entrypoint."""
import argparse
from pathlib import Path
import subprocess
import tempfile
import uuid


CHECK = '''import os, pathlib, signal, stat
try:
    assert os.getuid() != 0, "services still run as root"
    for name in ("config", "private", "state"):
        root = pathlib.Path("/kypost") / name
        info = root.stat()
        assert stat.S_IMODE(info.st_mode) == 0o700, "data root is not owner-only: " + name
        assert info.st_uid == os.getuid(), "data root has the wrong owner: " + name
    for path in ("/opt/kypost", "/opt/kypost/scripts/entrypoint.sh", "/opt/kypost/frontend"):
        assert not os.access(path, os.W_OK), "runtime user can rewrite executable assets"
    sentinel = pathlib.Path("/kypost/state/existing-mail")
    assert sentinel.read_bytes() == b"test-only retained bytes", "existing data changed"
    assert stat.S_IMODE(sentinel.stat().st_mode) == 0o640, "chmod changed descendant modes"
    for name in ("admin.env", "first-run-password.txt"):
        info = (pathlib.Path("/kypost/config") / name).stat()
        assert info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o600, "bootstrap secret metadata is unsafe"
    print("ok: real entrypoint protects mounted roots and retains existing data", flush=True)
except Exception as error:
    print("refused: " + str(error), flush=True)
    raise
finally:
    os.kill(1, signal.SIGTERM)
'''


def check(image):
    image_check = '''import pathlib, stat
for name in ("config", "private", "state"):
    assert stat.S_IMODE((pathlib.Path("/kypost") / name).stat().st_mode) == 0o700, "image root is not owner-only: " + name
print("ok: image data roots are owner-only")
'''
    subprocess.run(["docker", "run", "--rm", "--network", "none", "--user", "kypost", "--entrypoint", "python3", image, "-c", image_check], check=True, timeout=20)
    with tempfile.TemporaryDirectory(prefix="kypost-private-roots-") as directory:
        root = Path(directory)
        (root / "check.py").write_text(CHECK)
        (root / "supervisord.conf").write_text('''[supervisord]
nodaemon=true
loglevel=debug
logfile=/kypost/logs/supervisord.log
pidfile=/kypost/state/supervisord.pid
[program:check]
command=python3 /tmp/check-private-roots.py
startsecs=0
autorestart=false
stdout_logfile=NONE
stderr_logfile=NONE
''')
        # CI host and image UIDs may differ; these test inputs contain no secrets.
        for path in root.iterdir():
            path.chmod(0o644)
        name = "kypost-private-roots-" + uuid.uuid4().hex
        command = ["docker", "run", "--rm", "--name", name, "--network", "none", "-e", "KYPOST_BIND=127.0.0.1"]
        for source, target in (("check.py", "/tmp/check-private-roots.py"), ("supervisord.conf", "/etc/supervisord.conf")):
            command += ["--mount", f"type=bind,source={root / source},target={target},readonly"]
        # Only disposable container volumes are changed; no host mail data mounts.
        command += ["--entrypoint", "/bin/sh", image, "-ec", "chmod 0777 /kypost/config /kypost/private /kypost/state; printf 'test-only retained bytes' > /kypost/state/existing-mail; chmod 0640 /kypost/state/existing-mail; exec /opt/kypost/scripts/entrypoint.sh"]
        try:
            result = subprocess.run(command, capture_output=True, text=True, timeout=30, check=True)
            assert "ok: real entrypoint protects mounted roots and retains existing data" in result.stdout, result.stdout + result.stderr
            print("ok: real entrypoint protects mounted roots and retains existing data")
        finally:
            subprocess.run(["docker", "rm", "-f", "-v", name], capture_output=True, check=False, timeout=20)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image", help="locally built KyPost image")
    check(parser.parse_args().image)
