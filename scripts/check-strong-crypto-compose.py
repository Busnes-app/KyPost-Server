"""Prove the strong-key overlay only adds GODEBUG; renders Compose, starts nothing."""

import json
import os
from pathlib import Path
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
fixture = {
    "KYPOST_BIND": "127.0.0.1",
    "KYPOST_DNS": "192.0.2.53",
    "KYPOST_MADDY_BINARY": "/srv/qualified-maddy",
    "KYPOST_RECEIVING_TLS_DIR": "/srv/receiver-tls",
    "KYPOST_RECEIVING_HOSTNAME": "mail.example.com",
    "KYPOST_SMTP_BIND": "127.0.0.1",
    "KYPOST_SMTP_PORT": "2525",
    "TLS_CERT_FILE": "/kypost/private/tls.crt",
    "TLS_KEY_FILE": "/kypost/private/tls.key",
}
env = {k: v for k, v in os.environ.items() if not k.startswith(("COMPOSE_", "KYPOST_", "GODEBUG", "TLS_"))}

with tempfile.TemporaryDirectory(prefix="kypost-strong-crypto-") as directory:
    env_file = Path(directory) / ".env"
    env_file.write_text("".join(f"{k}={v}\n" for k, v in fixture.items()))

    def render(files):
        command = ["docker", "compose", "--project-directory", str(root), "--env-file", str(env_file)]
        for file in files:
            command += ["-f", str(root / file)]
        return json.loads(subprocess.check_output(command + ["config", "--format", "json"], env=env))

    chains = [
        [],
        ["docker-compose.lan-dns.yml"],
        ["docker-compose.rspamd.yml"],
        ["docker-compose.receiving.yml"],
        ["docker-compose.receiving.yml", "docker-compose.rspamd.yml", "docker-compose.lan-dns.yml"],
    ]
    for chain in chains:
        files = ["docker-compose.yml", *chain]
        baseline = render(files)
        configured = render([*files, "docker-compose.strong-crypto.yml"])
        assert "GODEBUG" not in baseline["services"]["kypost-server"].get("environment", {}), files
        assert configured["services"]["kypost-server"]["environment"].pop("GODEBUG") == "fips140=on", files
        assert configured == baseline, f"the strong-key overlay changed another setting in {files}"
print(f"All {len(chains)} Compose chains keep data, keys, image, network, DNS, receiving and Rspamd settings.")
