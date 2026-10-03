#!/usr/bin/env python3
"""Disposable Maddy 0.9.5 qualification; never starts a production receiver.

Usage: python3 scripts/check-receiving-gateway.py /path/to/pinned/maddy
Uses Python's standard library, a temporary spool and loopback high ports only.
"""
import hashlib
import json
import pathlib
import smtplib
import socket
import socketserver
import subprocess
import sys
import tempfile
import threading
import time


BINARY_SHA256 = "6ea4b951f15b91fd81d98957e4d4bad7a0cec6d6e1d66b011c765cc9a14e05db"
RAW = (b"From: sender@outside.test\r\nTo: visible@example.test\r\n"
       b"Message-ID: <gateway-proof@outside.test>\r\nSubject: MIME fidelity\r\n"
       b"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=proof\r\n"
       b"\r\n--proof\r\nContent-Type: text/plain\r\n\r\nhello\r\n.dot\r\n"
       b"--proof\r\nContent-Type: application/octet-stream\r\n"
       b"Content-Transfer-Encoding: base64\r\n\r\nAAEC/w==\r\n--proof--\r\n")
RECIPIENTS = ["visible@example.test", "hidden@example.test"]


def wait_for(check, message, seconds=30):
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        if check():
            return
        time.sleep(0.05)
    raise AssertionError(message)


class Pickup(socketserver.StreamRequestHandler):
    """Test SMTP sink: hold final ACK until the test permits queue deletion."""
    def handle(self):
        def reply(line):
            self.wfile.write(line + b"\r\n")
            self.wfile.flush()

        sender, recipients = None, []
        reply(b"220 pickup.example.test")
        while line := self.rfile.readline():
            command = line.upper()
            if command.startswith(b"EHLO") or command.startswith(b"HELO"):
                reply(b"250 pickup.example.test")
            elif command.startswith(b"MAIL FROM:"):
                sender = line.decode().strip().split(":", 1)[1].strip("<>")
                reply(b"250 sender accepted")
            elif command.startswith(b"RCPT TO:"):
                recipients.append(line.decode().strip().split(":", 1)[1].strip("<>"))
                reply(b"250 recipient accepted")
            elif command == b"DATA\r\n":
                reply(b"354 send data")
                chunks = []
                while (part := self.rfile.readline()) != b".\r\n":
                    if not part:
                        return
                    chunks.append(part[1:] if part.startswith(b"..") else part)
                self.server.deliveries.append((sender, recipients, b"".join(chunks)))
                self.server.received.set()
                if not self.server.ack.wait(40):
                    return
                # A real importer must commit durable mail/receipts before this ACK.
                reply(b"250 committed")
            elif command.startswith(b"QUIT"):
                reply(b"221 bye")
                return
            else:
                reply(b"250 OK")


def main():
    binary = pathlib.Path(sys.argv[1]).resolve()
    assert hashlib.sha256(binary.read_bytes()).hexdigest() == BINARY_SHA256, "unqualified binary"
    assert "0.9.5" in subprocess.check_output([str(binary), "version"], text=True)
    processes = []
    with tempfile.TemporaryDirectory(prefix="kypost-gateway-check-") as directory:
        root = pathlib.Path(directory)
        with socketserver.ThreadingTCPServer(("127.0.0.1", 0), Pickup) as pickup:
            pickup.daemon_threads = True
            pickup.deliveries = []
            pickup.received, pickup.ack = threading.Event(), threading.Event()
            threading.Thread(target=pickup.serve_forever, daemon=True).start()
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                port = reservation.getsockname()[1]
            (root / "state").mkdir()
            (root / "run").mkdir()
            config = root / "maddy.conf"
            config.write_text(f"""hostname receiver.example.test
state_dir {root}/state
runtime_dir {root}/run
log stderr
tls off
target.smtp pickup {{
    targets tcp://127.0.0.1:{pickup.server_address[1]}
    starttls no
}}
target.queue ingress {{
    target &pickup
}}
smtp tcp://127.0.0.1:{port} {{
    max_message_size 1M
    destination visible@example.test hidden@example.test {{
        deliver_to &ingress
    }}
    default_destination {{
        reject
    }}
}}
""")
            log = (root / "receiver.log").open("w+")

            def start():
                process = subprocess.Popen([str(binary), "--config", str(config), "run"],
                                           stdout=log, stderr=log)
                processes.append(process)

                def ready():
                    assert process.poll() is None, "receiver failed; " + (root / "receiver.log").read_text()
                    try:
                        with smtplib.SMTP("127.0.0.1", port, timeout=1):
                            return True
                    except OSError:
                        return False
                wait_for(ready, "receiver startup timed out")
                return process

            try:
                process = start()
                with smtplib.SMTP("127.0.0.1", port, timeout=5) as smtp:
                    smtp.mail("sender@outside.test")
                    for refused in ("unknown@example.test", "remote@outside.test"):
                        code, _ = smtp.rcpt(refused)
                        assert code >= 500, f"unexpected acceptance: {refused}"
                    smtp.rset()
                    try:
                        smtp.sendmail("sender@outside.test", RECIPIENTS, RAW + b"x" * (1024 * 1024))
                    except smtplib.SMTPResponseException as error:
                        assert error.smtp_code >= 500, "oversized message not permanently refused"
                    else:
                        raise AssertionError("oversized message accepted")
                    smtp.rset()
                    assert not smtp.sendmail("sender@outside.test", RECIPIENTS, RAW)
                wait_for(pickup.received.is_set, "pickup did not receive message")
                spool = root / "state" / "ingress"
                metadata_files = list(spool.glob("*.meta"))
                assert len(metadata_files) == 1, "accepted message missing from durable spool"
                metadata = json.loads(metadata_files[0].read_text())
                assert sorted(metadata["To"]) == sorted(RECIPIENTS)
                assert metadata["From"] == "sender@outside.test"
                delivery_id = metadata["MsgMeta"]["ID"]
                process.kill()  # Lost ACK / process crash after pickup, before commit acknowledgment.
                process.wait(timeout=5)
                assert metadata_files[0].exists(), "unacknowledged message lost at crash"
                pickup.received.clear()
                process = start()
                wait_for(lambda: len(pickup.deliveries) >= 2, "restart did not replay unacknowledged pickup")
                assert json.loads(metadata_files[0].read_text())["MsgMeta"]["ID"] == delivery_id
                for sender, recipients, delivered in pickup.deliveries:
                    assert sender == "sender@outside.test"
                    assert sorted(recipients) == sorted(RECIPIENTS), "Bcc envelope lost"
                    assert delivered.endswith(RAW), "original MIME bytes changed beyond prepended trace headers"
                    assert b"Bcc:" not in delivered and b"hidden@example.test" not in delivered
                    assert delivered[:-len(RAW)].startswith(b"Received:"), "unexpected prefix"
                pickup.ack.set()
                wait_for(lambda: not list(spool.glob("*.meta")), "ACK did not retire spool item")
                assert not list(spool.glob("*.body")), "ACK left message payload behind"
                print(json.dumps({"result": "pass", "receiver": "maddy 0.9.5", "attempts": len(pickup.deliveries),
                                  "checks": ["reject unknown recipient and relay", "reject oversized message", "durable envelope incl Bcc",
                                             "SIGKILL restart replay", "stable spool ID", "MIME suffix exact",
                                             "ACK retires spool"]}))
            finally:
                pickup.ack.set()
                for process in processes:
                    if process.poll() is None:
                        process.kill()
                    process.wait(timeout=5)
                pickup.shutdown()
                log.close()


if __name__ == "__main__":
    if not __debug__:
        raise SystemExit("Run without -O/PYTHONOPTIMIZE: qualification assertions must be enabled")
    main()
