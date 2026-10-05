#!/usr/bin/env python3
"""Qualify the pinned scanner/config on disposable, network-disabled state."""
import json
from email.utils import formatdate
import os
from pathlib import Path
import subprocess
import time
import uuid

root = Path(__file__).resolve().parent.parent
image = "rspamd/rspamd:3.14.3@sha256:b2fc96714bc4e376c87c5f2ee405f6edcc7acaa10b1ad04e023c4854ef47c6fc"

def run(args, data=None):
    result = subprocess.run(args, input=data, capture_output=True, timeout=45, cwd=root)
    if result.returncode:
        raise RuntimeError(f"{args[:3]} failed: {result.stderr.decode()[:1000]}")
    return result.stdout

env = os.environ.copy()
env.update(KYPOST_BIND="127.0.0.1", KYPOST_MADDY_BINARY="/tmp/maddy",
           KYPOST_RECEIVING_TLS_DIR="/tmp/tls", KYPOST_RECEIVING_HOSTNAME="mail.example.test",
           KYPOST_SMTP_BIND="127.0.0.1", KYPOST_SMTP_PORT="2525")
compose = subprocess.run(["docker", "compose", "-f", "docker-compose.yml", "-f",
                         "docker-compose.receiving.yml", "-f", "docker-compose.rspamd.yml",
                         "config", "--format", "json"], cwd=root, env=env,
                        capture_output=True, check=True, timeout=15)
cfg = json.loads(compose.stdout)
sidecar = cfg["services"]["rspamd"]
assert sidecar["image"] == image and sidecar["network_mode"] == "service:kypost-server"
assert sidecar["read_only"] and sidecar["user"] == "11333:11333"
assert not sidecar.get("ports") and sidecar["cap_drop"] == ["ALL"]
assert len(sidecar["volumes"]) == 1 and sidecar["volumes"][0]["read_only"]
assert cfg["services"]["kypost-server"]["environment"]["KYPOST_RECEIVING_RSPAMD"] == "true"

name = "kypost-rspamd-check-" + uuid.uuid4().hex[:12]
args = ["docker", "run", "--rm", "-d", "--name", name, "--network", "none", "--dns", "192.0.2.1",
        "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
        "--memory", "512m", "--pids-limit", "64",
        "--tmpfs", "/tmp:uid=11333,gid=11333,mode=0700,size=32m",
        "--tmpfs", "/var/lib/rspamd:uid=11333,gid=11333,mode=0700,size=32m",
        "--mount", f"type=bind,source={root}/scripts/rspamd/rspamd.conf,target=/etc/rspamd/rspamd.conf,readonly",
        "--entrypoint", "rspamd", image, "-f", "-c", "/etc/rspamd/rspamd.conf"]
try:
    run(args)
    dump = ["docker", "exec", name, "rspamadm", "configdump", "-c", "/etc/rspamd/rspamd.conf"]
    effective = run(dump).decode()
    assert not any("https://" in line or "http://" in line for line in effective.splitlines() if "description =" not in line), "unexpected remote map/service"
    workers = json.loads(run(dump + ["-j", "worker"]))
    assert set(workers) == {"normal"} and workers["normal"]["bind_socket"] == "127.0.0.1:11333"
    modules = run(dump + ["-m"]).decode().splitlines()
    enabled = next(line for line in modules if line.startswith("Modules enabled:")).partition(":")[2]
    assert set(enabled.strip().split(", ")) == {"mid", "chartable", "dkim", "maillist", "spf", "once_received", "hfilter", "mime_types", "dmarc", "regexp"}
    deadline = time.monotonic() + 30
    while True:
        ping = subprocess.run(["docker", "exec", name, "rspamc", "-h", "127.0.0.1:11333", "-t", "2", "-E"], capture_output=True, timeout=5)
        if ping.returncode == 0:
            break
        assert time.monotonic() < deadline, f"scanner failed to start: {ping.stdout.decode()[:300]} {ping.stderr.decode()[:300]}"
        time.sleep(0.1)
    sockets = run(["docker", "exec", name, "cat", "/proc/net/tcp", "/proc/net/tcp6"]).decode()
    listeners = [line.split()[1] for line in sockets.splitlines() if len(line.split()) > 3 and line.split()[3] == "0A"]
    assert listeners == ["0100007F:2C45"], listeners  # 11333, loopback only
    def scan(body):
        payload = ("Date: " + formatdate(usegmt=True) + "\r\nMessage-ID: <local-qualification@example.test>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nFrom: sender@example.test\r\nTo: one@example.test\r\nSubject: local qualification\r\n\r\n" + body + "\r\n").encode()
        return json.loads(run(["docker", "exec", "-i", name, "rspamc", "-j", "-h", "127.0.0.1:11333"], payload))
    clean = scan("Controlled ordinary message")
    spam = scan("XJS*C4JDBQADN1.NSBN3*2IDNEN*GTUBE-STANDARD-ANTI-UBE-TEST-EMAIL*C.34X")
    assert clean["action"] == "no action" and not clean["is_skipped"], clean
    assert spam["action"] == "reject", spam
    logs = run(["docker", "logs", name]).decode()
    assert "sender@example.test" not in logs and "local qualification" not in logs
    print("PASS: pinned nonroot sidecar, private scan-only socket, no remote services/maps, clean/GTUBE, private logging")
finally:
    subprocess.run(["docker", "rm", "-f", name], capture_output=True, timeout=20)
