#!/usr/bin/env python3
"""Prove launcher flags and unqualified executables fail before config creation."""
import hashlib
import os
import re
import signal
import time
from pathlib import Path
import subprocess
import tempfile

launcher = Path(__file__).resolve().with_name("start-receiving.sh")
assert os.getuid() != 0, "run this check as an unprivileged user"
with tempfile.TemporaryDirectory(prefix="kypost-launch-refusal-") as directory:
    root = Path(directory)
    config = root / "config"
    config.mkdir(mode=0o700)
    engine = root / "engine"
    engine.write_text("unqualified engine, never executed\n")
    engine.chmod(0o555)
    link = root / "link"
    link.symlink_to(engine)
    writable = root / "writable"
    writable.write_text(engine.read_text())
    writable.chmod(0o755)
    flags = ("KYPOST_NATIVE_RECEIVER", "KYPOST_NATIVE_MAIL", "KYPOST_NATIVE_RECEIVING")
    environment = {**os.environ, **dict.fromkeys(flags, "true"), "CONFIG_DIR": str(config), "KYPOST_RECEIVER_BINARY": str(engine)}
    cases = [({flag: "false"}, "enable all three native receiver flags") for flag in flags]
    cases += [({}, "engine hash differs"), ({"KYPOST_RECEIVER_BINARY": str(link)}, "regular nonsymlink file"), ({"KYPOST_RECEIVER_BINARY": str(writable)}, "regular nonsymlink file")]
    # An MX host (KYPOST_RECEIVING_REMOTE) needs the receiver flag and multi mode only.
    mx = {"KYPOST_RECEIVING_REMOTE": str(root / "remote.json"), "KYPOST_NATIVE_MAIL": "false", "KYPOST_NATIVE_RECEIVING": "false"}
    cases += [({**mx, "KYPOST_TENANCY": "single"}, "an MX host needs"), ({**mx, "KYPOST_TENANCY": "multi", "KYPOST_NATIVE_RECEIVER": "false"}, "an MX host needs"),
              ({**mx, "KYPOST_TENANCY": "multi"}, "engine hash differs")]
    for overrides, reason in cases:
        result = subprocess.run(["/bin/sh", str(launcher)], env={**environment, **overrides}, capture_output=True, text=True, timeout=5)
        assert result.returncode != 0 and reason in result.stderr, (reason, result.stdout, result.stderr)
        assert not list(config.iterdir()), "refused launcher wrote configuration"
print("ok: partial flags, wrong hash, symlink and writable engine refuse before configuration")

# The MX loop, on a copy of the launcher whose engine hash and tick are the only changes:
# a changed profile restarts the engine, an unchanged or failed fetch does not, a dead
# engine exits nonzero, and SIGTERM stops the engine and exits 0.
with tempfile.TemporaryDirectory(prefix="kypost-mx-loop-") as directory:
    root = Path(directory)
    config, bin_dir = root / "config", root / "bin"
    config.mkdir(mode=0o700)
    bin_dir.mkdir()
    starts, profile = root / "starts", root / "profile"
    engine = root / "engine"
    engine.write_text(f"#!/bin/sh\necho start >> {starts}\ntrap 'exit 0' TERM\nwhile :; do sleep 0.1; done\n")
    engine.chmod(0o555)
    helper = bin_dir / "kypost-server"
    helper.write_text(f"#!/bin/sh\n[ -f {profile} ] && cat {profile}\n")
    helper.chmod(0o555)
    digest = hashlib.sha256(engine.read_bytes()).hexdigest()
    text = launcher.read_text()
    qualified = re.search(r'" = ([0-9a-f]{64}) \]', text).group(1)
    assert text.count(qualified) == 1 and text.count("sleep 5 &") == 1
    copy = root / "launcher.sh"
    copy.write_text(text.replace(qualified, digest).replace("sleep 5 &", "sleep 0.05 &"))
    profile.write_text("v1\n")
    env = {**os.environ, "PATH": f"{bin_dir}:{os.environ['PATH']}", "CONFIG_DIR": str(config), "KYPOST_RECEIVER_BINARY": str(engine),
           "KYPOST_NATIVE_RECEIVER": "true", "KYPOST_TENANCY": "multi", "KYPOST_RECEIVING_REMOTE": str(root / "remote.json")}

    def wait_for(check, what):
        deadline = time.monotonic() + 10
        while not check():
            assert time.monotonic() < deadline, what
            time.sleep(0.05)

    count = lambda: len(starts.read_text().splitlines()) if starts.exists() else 0
    proc = subprocess.Popen(["/bin/sh", str(copy)], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    wait_for(lambda: count() == 1, "engine did not start")
    time.sleep(4)  # more than one 60-tick refresh with the same profile
    assert count() == 1, "an unchanged profile restarted the engine"
    profile.unlink()  # a failed fetch keeps the running profile
    time.sleep(4)
    assert count() == 1 and (config / "receiving.conf").read_text() == "v1\n", "a failed fetch changed the engine"
    profile.write_text("v2\n")
    wait_for(lambda: count() == 2, "a changed profile did not restart the engine")
    assert (config / "receiving.conf").read_text() == "v2\n"
    proc.send_signal(signal.SIGTERM)
    assert proc.wait(timeout=10) == 0, proc.stderr.read()
    assert not [p for p in config.iterdir() if p.name != "receiving.conf"], "temporary profile left behind"

    starts.unlink()
    proc = subprocess.Popen(["/bin/sh", str(copy)], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    wait_for(lambda: count() == 1, "engine did not start")
    for line in subprocess.run(["ps", "-o", "pid=,args=", "--ppid", str(proc.pid)], capture_output=True, text=True).stdout.splitlines():
        pid, _, args = line.strip().partition(" ")
        if "sleep" not in args:
            os.kill(int(pid), signal.SIGKILL)
    assert proc.wait(timeout=10) != 0 and "Maddy exited" in proc.stderr.read(), "a dead engine did not stop the launcher"
print("ok: the MX loop restarts only on a changed profile, keeps it on a failed fetch, exits with the engine and on SIGTERM")
