#!/usr/bin/env python3
"""Check the Docker shutdown budget and real Supervisor group stop order."""
import argparse
import configparser
import json
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import sys
import tempfile
import time


def check_contract(root):
    config = configparser.ConfigParser(interpolation=None)
    assert config.read(root / "supervisord.conf"), "missing supervisor contract"
    waits = sum(
        config.getint(section, "stopwaitsecs", fallback=10)
        for section in config.sections()
        if section.startswith(("program:", "eventlistener:"))
    )
    result = subprocess.run(
        ["docker", "compose", "-f", str(root / "docker-compose.yml"), "config", "--format", "json"],
        env={**os.environ, "KYPOST_BIND": "127.0.0.1"},
        capture_output=True, text=True, check=True,
    )
    duration = json.loads(result.stdout)["services"]["kypost-server"]["stop_grace_period"]
    parts = re.findall(r"(\d+)(h|m|s)", duration)
    assert "".join(value + unit for value, unit in parts) == duration, "unsupported shutdown duration"
    grace = sum(int(value) * {"h": 3600, "m": 60, "s": 1}[unit] for value, unit in parts)
    assert grace >= waits + 60, f"Docker grace {grace}s cannot cover serial Supervisor waits {waits}s plus 60s teardown"
    print(f"ok: Docker grace {grace}s covers serial waits {waits}s plus teardown")


def check_runtime():
    with tempfile.TemporaryDirectory(prefix="kypost-supervisor-stop-") as directory:
        root = Path(directory)
        events = root / "events.jsonl"
        worker = root / "worker.py"
        worker.write_text('''import json, os, signal, sys, time
def record(action):
    fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    try:
        os.write(fd, (json.dumps({"name": sys.argv[2], "action": action, "at": time.monotonic(), "pid": os.getpid()}) + "\\n").encode())
    finally:
        os.close(fd)
def drain(signum, frame):
    record("term")
    time.sleep(2)
    record("drained")
    sys.exit(0)
signal.signal(signal.SIGTERM, drain)
record("ready")
while True:
    signal.pause()
''')
        config = root / "supervisord.conf"
        text = f"[supervisord]\nnodaemon=true\nlogfile={root}/supervisor.log\npidfile={root}/supervisor.pid\n"
        for name in ("api", "daemon"):
            command = shlex.join([sys.executable, str(worker), str(events), name])
            text += f"\n[program:{name}]\ncommand={command}\npriority=10\nstartsecs=0\nautorestart=false\nstopwaitsecs=5\nstdout_logfile=NONE\nstderr_logfile=NONE\n"
        config.write_text(text)
        with (root / "console.log").open("w") as output:
            process = subprocess.Popen(
                [sys.executable, "-m", "supervisor.supervisord", "-c", str(config)],
                stdout=output, stderr=subprocess.STDOUT,
            )
            try:
                deadline = time.monotonic() + 10
                while True:
                    rows = [json.loads(line) for line in events.read_text().splitlines()] if events.exists() else []
                    if len([row for row in rows if row["action"] == "ready"]) == 2:
                        break
                    assert process.poll() is None and time.monotonic() < deadline, "Supervisor workers did not start"
                    time.sleep(0.05)
                process.send_signal(signal.SIGTERM)
                assert process.wait(timeout=15) == 0, "Supervisor shutdown failed"
                rows = [json.loads(line) for line in events.read_text().splitlines()]
                terms = sorted((row for row in rows if row["action"] == "term"), key=lambda row: row["at"])
                drained = {row["name"]: row["at"] for row in rows if row["action"] == "drained"}
                assert len(terms) == len(drained) == 2, "a scaled drain was interrupted"
                assert terms[1]["at"] >= drained[terms[0]["name"]], "separate Supervisor groups no longer stop serially"
                elapsed = max(drained.values()) - terms[0]["at"]
                assert elapsed >= 4, "two two-second serial drains did not complete"
                print(f"ok: same-priority groups drained serially in {elapsed:.2f}s; both completed")
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=15)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
                        # Only the test's recorded worker process groups are targets.
                        for row in rows:
                            if row["action"] == "ready":
                                try:
                                    os.killpg(row["pid"], signal.SIGKILL)
                                except ProcessLookupError:
                                    pass


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parent.parent)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--contract-only", action="store_true")
    mode.add_argument("--runtime-only", action="store_true")
    args = parser.parse_args()
    if not args.runtime_only:
        check_contract(args.root)
    if not args.contract_only:
        check_runtime()
