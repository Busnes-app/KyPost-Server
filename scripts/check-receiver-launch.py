#!/usr/bin/env python3
"""Prove launcher flags and unqualified executables fail before config creation."""
import os
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
    for overrides, reason in cases:
        result = subprocess.run(["/bin/sh", str(launcher)], env={**environment, **overrides}, capture_output=True, text=True, timeout=5)
        assert result.returncode != 0 and reason in result.stderr, (reason, result.stdout, result.stderr)
        assert not list(config.iterdir()), "refused launcher wrote configuration"
print("ok: partial flags, wrong hash, symlink and writable engine refuse before configuration")
