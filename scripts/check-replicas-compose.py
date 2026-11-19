#!/usr/bin/env python3
"""Render docker-compose.replicas.yml merges with synthetic inputs and check the replica
profile's contract; start no containers."""
import json
import os
import re
import subprocess
from pathlib import Path

root = Path(__file__).resolve().parents[1]
required = {
    "KYPOST_BIND": "127.0.0.1",
    "KYPOST_DATABASE_URL": "postgres://kypost:synthetic@postgres:5432/kypost?sslmode=disable",
    "KYPOST_POSTGRES_PASSWORD": "synthetic-postgres-password",
    "KYPOST_PROXY_IP": "172.31.251.2",
    "KYPOST_REPLICA_SUBNET": "172.31.251.0/24",
}
base_env = {k: v for k, v in os.environ.items() if not k.startswith("KYPOST_") and k not in ("TRUSTED_PROXY_CIDRS", "OLLAMA_BASE_URL")}
files = ["docker-compose.yml", "docker-compose.replicas.yml"]


def compose(chain, env):
    command = ["docker", "compose", "--env-file", "/dev/null"]
    for name in chain:
        command += ["-f", name]
    return subprocess.run(command + ["config", "--format", "json"], cwd=root, env=env, capture_output=True, text=True)


for missing in required:
    result = compose(files, base_env | {k: v for k, v in required.items() if k != missing})
    assert result.returncode != 0, f"rendered without {missing}"
    assert missing in result.stderr, f"error does not name {missing}: {result.stderr}"
print("PASS replicas: each required variable is named when missing")

result = compose(files, base_env | required | {"KYPOST_REPLICAS": "3"})
assert result.returncode == 0, result.stderr
doc = json.loads(result.stdout)
services = doc["services"]
app, db, proxy, ollama = services["kypost-server"], services["postgres"], services["proxy"], services["ollama"]
assert app["deploy"]["replicas"] == 3, "KYPOST_REPLICAS not applied"
assert proxy["environment"]["KYPOST_REPLICAS"] == "3", "HAProxy has a slot count other than KYPOST_REPLICAS"
assert "container_name" not in app, "a fixed container name allows one replica"
assert not app.get("ports"), "an app replica publishes a host port"
shared = sorted((v["source"], v["target"]) for v in app["volumes"] if v.get("source"))
assert shared == [("kypost_config", "/kypost/config"), ("kypost_private", "/kypost/private"), ("kypost_state", "/kypost/state")], f"shared app mounts {shared}"
anonymous = [v["target"] for v in app["volumes"] if not v.get("source")]
assert anonymous == ["/kypost/logs"] and len(app["volumes"]) == 4, f"app mounts {app['volumes']}, want the three data volumes and a per-container logs volume"
assert set(app["networks"]) == {"replicas", "database"}, "an app replica pins an address or joins kypost-net"
env = app["environment"]
assert env["TRUSTED_PROXY_CIDRS"] == required["KYPOST_PROXY_IP"] + "/32", env["TRUSTED_PROXY_CIDRS"]
assert env["OLLAMA_BASE_URL"] == "http://ollama:11434", "replicas start their own Ollama"
assert env["KYPOST_DATABASE_URL"] == required["KYPOST_DATABASE_URL"] and env["KYPOST_DATABASE_URL_FILE"] == ""
print("PASS replicas: KYPOST_REPLICAS=3, no fixed name or app port, three shared volumes plus per-container logs, proxy-only trust, one Ollama")

assert "container_name" not in db and not db.get("ports"), "PostgreSQL is reachable from the host"
assert set(db["networks"]) == {"database"} and doc["networks"]["database"].get("internal") is True, "PostgreSQL is not on an internal network only"
assert db["environment"]["POSTGRES_PASSWORD"] == required["KYPOST_POSTGRES_PASSWORD"], "PostgreSQL password is not the operator's"
assert db["environment"]["POSTGRES_INITDB_ARGS"] == "--locale=C --encoding=UTF8"
assert re.fullmatch(r"postgres:17[\w.-]*@sha256:[0-9a-f]{64}", db["image"]), db["image"]
for name, svc in [("proxy", proxy), ("ollama", ollama)]:
    assert re.search(r"@sha256:[0-9a-f]{64}$", svc["image"]), f"{name} image is not pinned by digest"
assert [p["host_ip"] for p in proxy["ports"]] == [required["KYPOST_BIND"]] and proxy["ports"][0]["target"] == 5866, "KYPOST_BIND does not gate the proxy publish"
assert proxy["networks"]["replicas"]["ipv4_address"] == required["KYPOST_PROXY_IP"]
print("PASS replicas: PostgreSQL 17 digest-pinned, C/UTF8, internal network, no host port, operator password; KYPOST_BIND gates the proxy")

# No service carries the database URL in argv; the replicas read it from their environment.
for name, svc in services.items():
    argv = json.dumps([svc.get("command"), svc.get("entrypoint")])
    assert "postgres://" not in argv and "postgresql://" not in argv, f"{name} has a database URL in argv"
print("PASS replicas: the database URL is never in an argv")

# HAProxy: /readyz every second, two fails out, two passes in; the timeouts are the docs'
# proxy table, each above the route's own budget.
cfg = (root / "deploy/replicas/haproxy.cfg").read_text()
assert "http-check send meth GET uri /readyz" in cfg and "inter 1s fall 2 rise 2" in cfg, "HAProxy does not probe /readyz each second, 2 fails / 2 passes"
assert "http-request disable-l7-retry unless METH_GET || METH_HEAD" in cfg and "retry-on conn-failure empty-response" in cfg
assert re.search(r"(?m)^\s+timeout server 15s$", cfg), "ordinary requests need a server timeout above the 14 s deadline"
table = dict(re.findall(r"(?m)^\| `([^`]+)` \| [0-9]+ (?:s|min) \| ([0-9]+) min \|", (root / "docs/RUNNING.md").read_text()))
assert table, "no proxy-timeout table in docs/RUNNING.md"
rules = {}
for minutes, prefixes in re.findall(r"set-timeout server (\d+)m if \{ path_beg ([^}]+)\}", cfg):
    for prefix in prefixes.split():
        rules[prefix] = minutes
assert rules == table, f"haproxy set-timeout rules {rules} differ from the docs table {table}"
print("PASS replicas: HAProxy probes /readyz and its timeouts are the proxy table")

for overlay in ["docker-compose.receiving.yml", "docker-compose.rspamd.yml", "docker-compose.lan-dns.yml"]:
    chain = ["docker-compose.yml", overlay, "docker-compose.replicas.yml"]
    extra = {"KYPOST_RECEIVING_HOSTNAME": "mx.example.test", "KYPOST_SMTP_BIND": "127.0.0.1", "KYPOST_SMTP_PORT": "25",
             "KYPOST_MADDY_BINARY": "/x", "KYPOST_RECEIVING_TLS_DIR": "/x", "KYPOST_DNS": "192.0.2.53"}
    result = compose(chain, base_env | required | extra)
    assert result.returncode != 0 and overlay in result.stderr, f"{overlay} rendered with replicas, or its refusal does not name it: {result.stderr}"
    alone = compose(["docker-compose.yml", overlay], base_env | {"KYPOST_BIND": "127.0.0.1"} | extra)
    assert alone.returncode == 0, f"{overlay} alone no longer renders: {alone.stderr}"
print("PASS replicas: receiving, rspamd and lan-dns overlays refuse to render with replicas")
