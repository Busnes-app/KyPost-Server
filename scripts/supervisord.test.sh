#!/usr/bin/env bash
# Self-check for supervisord.conf: it parses, and no two supervised streams
# share a log file.
#
# supervisord opens and rotates each program's stream independently, so two
# programs pointed at one path rename each other's file out from under a still
# open handle — the lines written after that go to an inode nobody reads and
# are discarded at the next rotation. [program:ollama] and [program:ollama-model]
# shared one, which lost exactly the model-pull retry diagnostics an operator
# opens when the classifier model never installs.
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "$root_dir/supervisord.conf" <<'PY'
import collections, configparser, pathlib, subprocess, sys

path = sys.argv[1]
cfg = configparser.ConfigParser(strict=True, inline_comment_prefixes=(";",))
cfg.read(path)

owners = collections.defaultdict(list)
for section in cfg.sections():
    for key in ("logfile", "stdout_logfile", "stderr_logfile"):
        value = cfg[section].get(key)
        if value and value not in ("AUTO", "NONE"):
            owners[value].append(f"[{section}] {key}")

shared = {p: o for p, o in owners.items() if len(o) > 1}
if shared:
    for p, o in sorted(shared.items()):
        print(f"shared log file {p}: {', '.join(o)}", file=sys.stderr)
    sys.exit(1)

print(f"ok: {len(cfg.sections())} sections, {len(owners)} distinct log files")

# Replace only the final signal in the test copy, so this check cannot kill
# the host's PID 1. Everything else uses the real listener and event protocol.
listener = pathlib.Path(path).parent / "scripts" / "crash-exit.sh"
script = listener.read_text()
signal = "kill -TERM 1"
assert script.count(signal) == 1, "review the listener's shutdown stub"
script = script.replace(signal, "printf 'container stopped\\n' >&2")

def fatal(name):
    payload = f"processname:{name} groupname:{name} from_state:BACKOFF"
    return f"ver:3.0 eventname:PROCESS_STATE_FATAL len:{len(payload)}\n{payload}"

for name in ("api", "daemon", "receiver", "ollama", "ollama-model", "receiving-server-other"):
    result = subprocess.run(
        ["sh", "-c", script], input=fatal("receiving-server") + fatal(name),
        capture_output=True, text=True, check=True, timeout=5,
    )
    assert result.stdout == "READY\nRESULT 2\nOKREADY\nRESULT 2\nOK", (
        "optional receiving-server must acknowledge FATAL and keep listening", result
    )
    assert result.stderr.count("container stopped\n") == 1, result
    assert f"processname:{name} " in result.stderr, result

result = subprocess.run(
    ["sh", "-c", script], input=fatal("receiving-server"),
    capture_output=True, text=True, check=True, timeout=5,
)
assert "container stopped" not in result.stderr, result
print("ok: optional accelerator FATAL keeps the container up; core FATAL still stops it")
result = subprocess.run(["sh", "-c", script], input=fatal("startfailure"), capture_output=True, text=True, check=True, timeout=5)
assert "container stopped" not in result.stderr and result.stdout == "READY\nRESULT 2\nOKREADY\n", result
print("ok: a failed startfailure listener never stops the container")

# A refused start reaches the container's own stderr: the listener copies the end of the
# program's error log there (supervisord points its stderr at /dev/fd/2).
import os, tempfile
assert cfg["eventlistener:startfailure"]["stderr_logfile"] == "/dev/fd/2", "start refusals must reach docker logs"
assert cfg["eventlistener:startfailure"]["events"] == "PROCESS_STATE_BACKOFF"
starts = (pathlib.Path(path).parent / "scripts" / "start-failure.sh").read_text()
def backoff(name):
    payload = f"processname:{name} groupname:{name} from_state:STARTING tries:1"
    return f"ver:3.0 eventname:PROCESS_STATE_BACKOFF len:{len(payload)}\n{payload}"
with tempfile.TemporaryDirectory() as logs:
    pathlib.Path(logs, "api.err.log").write_text("\n".join(f"line {i}" for i in range(9)) + "\nsettings differ from the deployment's: SERVER_BASE_URL\n")
    result = subprocess.run(["sh", "-c", starts], input=backoff("api") + backoff("ollama"),
        capture_output=True, text=True, check=True, timeout=5, env=os.environ | {"KYPOST_LOG_DIR": logs})
assert result.stdout == "READY\nRESULT 2\nOKREADY\nRESULT 2\nOKREADY\n", result
assert "kypost api did not start" in result.stderr and "SERVER_BASE_URL" in result.stderr, result
assert "line 4" not in result.stderr and "ollama" not in result.stderr, result
print("ok: a refused API or daemon start shows its reason on the container's stderr")
PY
