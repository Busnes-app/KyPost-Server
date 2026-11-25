#!/usr/bin/env python3
"""Render docker-compose.replicas.yml merges with synthetic inputs and check the replica
profile's contract; start no containers."""
import base64
import hashlib
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
    "KYPOST_PROXY_IP": "10.89.1.2",
    "KYPOST_REPLICA_SUBNET": "10.89.1.0/24",
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
assert not any((n or {}).get("ipv4_address") for n in app["networks"].values()), "an app replica pins an address"
env = app["environment"]
assert env["TRUSTED_PROXY_CIDRS"] == required["KYPOST_PROXY_IP"] + "/32", env["TRUSTED_PROXY_CIDRS"]
assert env["OLLAMA_BASE_URL"] == "http://ollama:11434", "replicas start their own Ollama"
assert env["KYPOST_DATABASE_URL"] == required["KYPOST_DATABASE_URL"] and env["KYPOST_DATABASE_URL_FILE"] == ""
print("PASS replicas: KYPOST_REPLICAS=3, no fixed name or app port, three shared volumes plus per-container logs, proxy-only trust, one Ollama")

assert "container_name" not in db and not db.get("ports"), "PostgreSQL is reachable from the host"
assert set(db["networks"]) == {"database"} and doc["networks"]["database"].get("internal") is True, "PostgreSQL is not on an internal network only"
assert db["environment"]["POSTGRES_PASSWORD"] == required["KYPOST_POSTGRES_PASSWORD"], "PostgreSQL password is not the operator's"
assert db["environment"]["POSTGRES_INITDB_ARGS"] == "--locale=C --encoding=UTF8"
# PostgreSQL 18, moving a 17 volume on the first up (scripts/postgres-17-to-18.sh, the shared
# Ky kit, byte for byte; scripts/test-postgres-17-to-18.sh runs it).
kit = "/ky/postgres-17-to-18.sh"
assert hashlib.sha256((root / "scripts/postgres-17-to-18.sh").read_bytes()).hexdigest() == "a800e026af73d9304aa970c2c3919caecd4c52eb8d9b702f3447e632bdc21214", \
    "scripts/postgres-17-to-18.sh differs from the shared Ky kit"
mounts = lambda svc: sorted((v.get("source", "").rsplit("/scripts/", 1)[-1], v["target"], bool(v.get("read_only"))) for v in svc["volumes"])
assert db["image"] == "postgres:18.7-alpine3.24@sha256:0115267ab5a509022df9319c77be365cef335adb71556c75f60f5360ee247e71", db["image"]
assert "PGDATA" not in db["environment"], "PGDATA set: 18 keeps its data in /var/lib/postgresql/18/docker"
assert mounts(db) == [("kypost_postgres", "/var/lib/postgresql", False), ("postgres-17-to-18.sh", kit, True)], f"PostgreSQL 18 mounts {db['volumes']}"
assert db["entrypoint"] == ["bash", kit, "restore"] and db["command"][0] == "postgres", (db["entrypoint"], db["command"])
assert db["depends_on"]["postgres-17-dump"]["condition"] == "service_completed_successfully", "18 starts before the 17 dump finished"
assert db["healthcheck"]["start_period"] == "1h0m0s", "a long restore would mark PostgreSQL unhealthy"
dump = services["postgres-17-dump"]
assert dump["image"] == "postgres:17.12-alpine3.24@sha256:f9cf3e1f0066f0eb0c48707d864f0ac5941e9ff454a072077d97a4d278d07d73", dump["image"]
assert dump["entrypoint"] == ["bash", kit, "dump"] and dump["network_mode"] == "none" and dump["restart"] == "no", "the 17 dump is not an offline one-shot"
assert dump["environment"] == {"POSTGRES_USER": db["environment"]["POSTGRES_USER"], "OLD": "/var/lib/postgresql/data", "NEW": "/pg18"}, dump["environment"]
assert mounts(dump) == [("kypost_pgdata", "/var/lib/postgresql/data", False), ("kypost_postgres", "/pg18", False), ("postgres-17-to-18.sh", kit, True)], f"17 dump mounts {dump['volumes']}"
assert not any("kypost_pgdata" == v.get("source") for name, svc in services.items() if name != "postgres-17-dump" for v in svc.get("volumes", [])), \
    "only the 17 dump may mount the 17 volume"
for name, svc in [("proxy", proxy), ("ollama", ollama)]:
    assert re.search(r"@sha256:[0-9a-f]{64}$", svc["image"]), f"{name} image is not pinned by digest"
assert [p["host_ip"] for p in proxy["ports"]] == [required["KYPOST_BIND"]] and proxy["ports"][0]["target"] == 5866, "KYPOST_BIND does not gate the proxy publish"
assert proxy["networks"]["replicas"]["ipv4_address"] == required["KYPOST_PROXY_IP"]
print("PASS replicas: PostgreSQL 18 digest-pinned, the 17 volume moved by the kit's one-shot dump and restore, C/UTF8, internal network, no host port, operator password; KYPOST_BIND gates the proxy")

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
# A docs route with "*" (one path segment) is a path_reg rule.
for minutes, pattern in re.findall(r"set-timeout server (\d+)m if \{ path_reg \^([^ ]+)\$ \}", cfg):
    rules[pattern.replace("[^/]+", "*")] = minutes
assert rules == table, f"haproxy set-timeout rules {rules} differ from the docs table {table}"
print("PASS replicas: HAProxy probes /readyz and its timeouts are the proxy table")

# Only the edge proxy may name the client or the scheme: HAProxy drops those headers from
# every other source, and with no edge the ACL matches no TCP source at all.
assert 'acl from_edge src "${KYPOST_EDGE_TRUSTED_PROXY}"' in cfg, "HAProxy has no edge-proxy ACL"
for header in ("X-Forwarded-For", "X-Forwarded-Proto"):
    assert f"http-request del-header {header} unless from_edge" in cfg, f"HAProxy passes a client's {header} through"
assert "http-request set-header CF-Connecting-IP %[src] unless from_edge" in cfg, "HAProxy passes a client's CF-Connecting-IP through"
edge = proxy["environment"].get("KYPOST_EDGE_TRUSTED_PROXY")
assert edge == "255.255.255.255", f"with no edge set HAProxy must trust no source, got {edge}"
print("PASS replicas: HAProxy keeps forwarded client headers only from the edge proxy")

# With no replica ready, HAProxy answers with KyPost's page: not signed out, retry soon.
page = (root / "deploy/replicas/503.http").read_bytes()
assert "errorfile 503 /usr/local/etc/haproxy/503.http" in cfg and page.startswith(b"HTTP/1.1 503 ") and b"Retry-After:" in page and b"You have not been signed out" in page
head, body = page.split(b"\n\n", 1)
assert f"Content-Length: {len(body)}".encode() in head, "503.http Content-Length is wrong"
assert b'<meta name="color-scheme" content="light dark">' in body, "503.http does not follow the browser's dark mode"
inline = re.search(rb"<style>(.*?)</style>", body).group(1)
digest = base64.b64encode(hashlib.sha256(inline).digest())
assert b"style-src 'sha256-" + digest + b"'" in head, "503.http's CSP hash does not match its style"
print("PASS replicas: HAProxy's 503 is KyPost's unavailable page")

# TLS ends in front of HAProxy: a certificate on the replicas is refused, by name.
result = compose(files, base_env | required | {"TLS_CERT_FILE": "/kypost/config/tls/cert.pem"})
assert result.returncode != 0 and "TLS_CERT_FILE cannot be used with docker-compose.replicas.yml" in result.stderr, f"TLS_CERT_FILE rendered with replicas: {result.stderr}"
sentinel = re.search(r"TLS_CERT_FILE: \$\{TLS_CERT_FILE:\+\$\{([A-Z_]+):\?", (root / "docker-compose.replicas.yml").read_text()).group(1)
assert sentinel not in (root / ".env.example").read_text() and sentinel not in (root / "docs/RUNNING.md").read_text(), f"{sentinel} must never be offered as a setting"
assert app["environment"].get("TLS_CERT_FILE", "") == "", "replicas would serve TLS that HAProxy cannot read"
print("PASS replicas: TLS_CERT_FILE is refused with a message")

# A TLS proxy configured for the single container (http://KyPost-Server:5866 or KYPOST_IP)
# reaches HAProxy after the cutover.
net = proxy["networks"]["kypost-net"]
assert "KyPost-Server" in (net.get("aliases") or []) and net.get("ipv4_address"), f"HAProxy does not answer as KyPost-Server on kypost-net: {net}"
assert 'server-template app "${KYPOST_REPLICAS}" kypost-replica:5866' in cfg and "kypost-replica" in (app["networks"]["replicas"].get("aliases") or []), \
    "HAProxy must find the replicas by a name its own KyPost-Server alias cannot match (DNS ignores case)"
print("PASS replicas: HAProxy answers as KyPost-Server at KYPOST_IP on kypost-net")

# The docs' commands are the ones the cutover check runs.
running = (root / "docs/RUNNING.md").read_text()
cutover = (root / "scripts/check-replica-cutover.sh").read_text()
assert "replicas.yml up -d --wait postgres" in running, "RUNNING.md starts the database without --wait; the next command would fail"
assert "docker compose pull && docker compose stop kypost-server && docker compose up -d --wait" in running, "RUNNING.md's upgrade does not pull, then stop every replica"
step4 = ('if [ -e /restore/keep.tar ]; then echo "restore/keep.tar already exists: nothing was changed. To try again, roll back first, '
         'then follow \\"To try again\\" below the rollback." >&2; exit 1; fi; umask 077 && tar -cf /restore/keep.tar.part -C /kypost config private state && '
         'mv /restore/keep.tar.part /restore/keep.tar && for r in config private state; do find "/kypost/$r" -mindepth 1 -delete && '
         'cp -pR "/restore/recovered/$r/." "/kypost/$r/" || exit 1; done')
rollback = ('tar -tf /restore/keep.tar >/dev/null && for r in config private state; do find "/kypost/$r" -mindepth 1 -delete || exit 1; done && '
            'tar -xpf /restore/keep.tar -C /kypost')
for step in ("createdb -U kypost kypost_restored", "/usr/local/bin/kypost-server restore /restore/backup.kycap /restore/recovered",
             "umask 077 && cp \"$(ls -t /kypost/state/backups/*.kycap | head -n 1)\" /restore/backup.kycap", step4, rollback,
             "dropdb -U kypost kypost_restored"):
    assert step in running and step in cutover, f"the replica restore step {step!r} is not both documented and run by the cutover check"
# keep.tar holds every private key: a private folder, written under umask 077, checked 600 by
# the cutover, and one guarded command so a failed or repeated copy never empties the volumes.
assert "mkdir -p -m 700 restore" in running, "RUNNING.md must create ./restore private (mode 700), rerunnably"
assert "sudo install -o 1000 -g 1000 -m 600" in running and running.index("mkdir -p -m 700 restore") < running.index("sudo install -o 1000 -g 1000 -m 600"), \
    "a capsule copied in by another user is installed as kypost, mode 600, after ./restore exists"
# After a rollback, trying again starts over with every replica stopped; the host-side moves
# are documented here (the cutover does the same inside its volumes).
for line in ("**To try again**, with every replica stopped", "sudo mv restore/keep.tar keep.tar.first",
             "sudo rm -rf restore/recovered restore/.kypost-restore-*", "then start again at step 2"):
    assert line in running and running.index(line) > running.index("**If the restore does not check out**"), f"RUNNING.md's 'To try again' lacks {line!r}"
assert "run **Back up now**" in running and running.index("run **Back up now**") < running.index("sudo rm -rf restore"), "RUNNING.md must say to back up before removing ./restore"
assert 'test "$(stat -c %a /restore/keep.tar)" = 600' in cutover, "the cutover must check keep.tar is mode 600"
assert "remove `./restore`" in running, "RUNNING.md must say to remove ./restore (it holds every private key unencrypted)"
print("PASS replicas: RUNNING.md's start, upgrade and restore commands match what the cutover check runs")

# A TLS proxy on the host cannot be the edge: the docs say so in every place that offers it.
proxies = (root / "docs/Reverse_Proxy_Networking.md").read_text()
host_row = next(line for line in proxies.splitlines() if "on the host, via `127.0.0.1:5866`" in line)
assert "single container only" in host_row, "Reverse_Proxy_Networking.md offers a host proxy without 'single container only'"
bind = next(line for line in (root / "docker-compose.replicas.yml").read_text().splitlines() if "KYPOST_BIND:?" in line)
assert "proxy on this host" not in bind and "container on kypost-net" in bind, f"the overlay's KYPOST_BIND message offers a host proxy: {bind}"
# Compose interpolates the base file first, so its message is the one an admin sees.
base_bind = next(line for line in (root / "docker-compose.yml").read_text().splitlines() if "KYPOST_BIND:?" in line)
assert "single container only; with replicas see docs/RUNNING.md" in base_bind, f"the base KYPOST_BIND message offers a host proxy to replicas: {base_bind}"
assert "A TLS proxy on the host" in running, "RUNNING.md must tell a host-proxy admin what to do"
print("PASS replicas: a TLS proxy on the host is single-container only, everywhere it is offered")

for overlay in ["docker-compose.receiving.yml", "docker-compose.rspamd.yml", "docker-compose.lan-dns.yml"]:
    chain = ["docker-compose.yml", overlay, "docker-compose.replicas.yml"]
    extra = {"KYPOST_RECEIVING_HOSTNAME": "mx.example.test", "KYPOST_SMTP_BIND": "127.0.0.1", "KYPOST_SMTP_PORT": "25",
             "KYPOST_MADDY_BINARY": "/x", "KYPOST_RECEIVING_TLS_DIR": "/x", "KYPOST_DNS": "192.0.2.53"}
    result = compose(chain, base_env | required | extra)
    assert result.returncode != 0 and overlay in result.stderr, f"{overlay} rendered with replicas, or its refusal does not name it: {result.stderr}"
    alone = compose(["docker-compose.yml", overlay], base_env | {"KYPOST_BIND": "127.0.0.1"} | extra)
    assert alone.returncode == 0, f"{overlay} alone no longer renders: {alone.stderr}"
assert "Replicas have no native receiving" in running and "Cloudflare and external-IMAP profiles work" not in running, \
    "RUNNING.md must say replicas have no native receiving: Cloudflare pickup needs the Rspamd overlay, which refuses replicas"
print("PASS replicas: receiving, rspamd and lan-dns overlays refuse to render with replicas, as RUNNING.md says")

# The hosted receiving profile (multi-tenant): docker-compose.hosted.yml after the replica
# chain, and docker-compose.mx.yml alone on an MX host. Rendered only; multi mode cannot
# start until the tenancy design's PR 7.
hosted_files = files + ["docker-compose.hosted.yml"]
hosted = {"KYPOST_STAFF_ORG_ID": "6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d", "KYPOST_RECEIVING_SUBNET": "10.89.2.0/24",
          "KYPOST_RECEIVING_A_IP": "10.89.2.10", "KYPOST_RECEIVING_B_IP": "10.89.2.11", "KYPOST_RECEIVING_PUBLISH_IP": "192.168.40.5"}
for missing in hosted:
    result = compose(hosted_files, base_env | required | {k: v for k, v in hosted.items() if k != missing})
    assert result.returncode != 0 and missing in result.stderr, f"hosted profile rendered without {missing}: {result.stderr}"
result = compose(hosted_files, base_env | required | hosted)
assert result.returncode == 0, result.stderr
services = json.loads(result.stdout)["services"]
pin = re.search(r"^\s*image:\s*(rspamd/rspamd:\S+@sha256:[0-9a-f]{64})\s*$", (root / "docker-compose.rspamd.yml").read_text(), re.M).group(1)
replica_env = services["kypost-server"]["environment"]
assert replica_env["KYPOST_TENANCY"] == "multi" and replica_env["KYPOST_NATIVE_RECEIVING"] == "true", "replicas do not import in multi mode"
assert replica_env.get("KYPOST_NATIVE_RECEIVER") in (None, "false"), "a replica runs Maddy"
for name, ip, port in [("receiving-a", hosted["KYPOST_RECEIVING_A_IP"], "2601"), ("receiving-b", hosted["KYPOST_RECEIVING_B_IP"], "2602")]:
    rx, spam = services[name], services["rspamd-" + name[-1]]
    assert rx["entrypoint"] == ["/usr/local/bin/kypost-server", "receiving", "serve", "--tls"] and rx["command"] == [ip + ":2600"], (name, rx["entrypoint"], rx["command"])
    assert rx["networks"]["receiving"]["ipv4_address"] == ip, f"{name} listens on an address it does not hold"
    assert [(p["host_ip"], p["published"], p["target"]) for p in rx["ports"]] == [(hosted["KYPOST_RECEIVING_PUBLISH_IP"], port, 2600)], f"{name} publishes {rx['ports']}"
    assert rx["read_only"] and rx["cap_drop"] == ["ALL"] and rx["user"] == "1000:1000", f"{name} is not read-only, capability-free and unprivileged"
    assert rx["environment"]["KYPOST_TENANCY"] == "multi" and rx["environment"]["KYPOST_RECEIVING_RSPAMD"] == "true"
    assert not any("postgres://" in a for a in rx["entrypoint"] + rx["command"]), f"{name} has the database URL in argv"
    assert sorted((v["source"], v["target"]) for v in rx["volumes"]) == shared, f"{name} does not share the three data volumes"
    assert spam["network_mode"] == "service:" + name and spam["image"] == pin, f"rspamd-{name[-1]} is not {name}'s own pinned scanner"
    assert spam["read_only"] and spam["cap_drop"] == ["ALL"] and not spam.get("ports"), f"rspamd-{name[-1]} is exposed or writable"
print("PASS hosted: two receiving servers on the shared volumes, each with its own pinned Rspamd, published only on the private address")

mx_env = {"KYPOST_STAFF_ORG_ID": hosted["KYPOST_STAFF_ORG_ID"], "KYPOST_RECEIVING_HOSTNAME": "mx-a.example.test", "KYPOST_SMTP_BIND": "203.0.113.25",
          "KYPOST_MADDY_BINARY": "/opt/maddy", "KYPOST_RECEIVING_TLS_DIR": "/etc/mx-tls", "KYPOST_MX_DIR": "/etc/kypost-mx"}
for missing in mx_env:
    result = compose(["docker-compose.mx.yml"], base_env | {k: v for k, v in mx_env.items() if k != missing})
    assert result.returncode != 0 and missing in result.stderr, f"MX profile rendered without {missing}: {result.stderr}"
result = compose(["docker-compose.mx.yml"], base_env | mx_env)
assert result.returncode == 0, result.stderr
doc = json.loads(result.stdout)
assert list(doc["services"]) == ["mx"] and not doc.get("volumes"), "an MX host runs more than Maddy or keeps a volume"
mx = doc["services"]["mx"]
assert all(v["type"] == "bind" and v["read_only"] for v in mx["volumes"]), "an MX host mounts something writable"
assert mx["read_only"] and mx["cap_drop"] == ["ALL"] and mx["init"] and mx["user"] == "1000:1000"
assert mx["environment"]["KYPOST_RECEIVING_REMOTE"] == "/kypost/mx/remote.json" and mx["environment"]["KYPOST_TENANCY"] == "multi"
assert not any(k in mx["environment"] for k in ("KYPOST_DATABASE_URL", "KYPOST_NATIVE_MAIL", "KYPOST_RECEIVING_RSPAMD")), "an MX host is given state or a scanner"
assert [(p["host_ip"], p["published"], p["target"]) for p in mx["ports"]] == [(mx_env["KYPOST_SMTP_BIND"], "25", 2525)], mx["ports"]
print("PASS hosted: the MX host holds nothing, mounts read-only, and forwards to the replicas")
