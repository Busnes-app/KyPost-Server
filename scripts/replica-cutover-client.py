#!/usr/bin/env python3
"""HTTP steps for check-replica-cutover.sh (stdlib only): sign in as the browser does, change
the first-run password, pin a recovery key, back up, and read the session and backup status.
Never prints a password or secret; session-cookie prints the throwaway test session."""
import base64
import hashlib
import hmac
import http.cookiejar
import json
import os
import sys
import urllib.error
import urllib.request


def derive(password, salt_b64, iterations):
    """users.DeriveAuthSecret: PBKDF2-SHA256 then HKDF-SHA256 (empty salt, kypost/auth/v1)."""
    stretch = hashlib.pbkdf2_hmac("sha256", password.encode(), base64.b64decode(salt_b64), iterations, 32)
    prk = hmac.new(b"\0" * 32, stretch, hashlib.sha256).digest()
    return hmac.new(prk, b"kypost/auth/v1\x01", hashlib.sha256).digest().hex()


class Client:
    def __init__(self, base, jar_path):
        self.base, self.jar_path = base.rstrip("/"), jar_path
        self.jar = http.cookiejar.MozillaCookieJar(jar_path)
        if os.path.exists(jar_path):
            self.jar.load(ignore_discard=True, ignore_expires=True)
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))

    def call(self, method, path, body=None):
        headers = {"Content-Type": "application/json"}
        for c in self.jar:
            if c.name == "csrf_token":
                headers["X-CSRF-Token"] = c.value
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method, headers=headers)
        try:
            with self.opener.open(req, timeout=120) as resp:
                status, raw = resp.status, resp.read()
        except urllib.error.HTTPError as e:
            status, raw = e.code, e.read()
        self.jar.save(ignore_discard=True, ignore_expires=True)
        try:
            return status, json.loads(raw or b"{}")
        except ValueError:
            return status, {}

    def secret(self, username, password):
        status, params = self.call("GET", "/api/auth/login-params?username=" + username)
        assert status == 200, f"login-params: {status}"
        return derive(password, params["salt"], params["iterations"]), params


def main():
    command, base, jar = sys.argv[1], sys.argv[2], sys.argv[3]
    c = Client(base, jar)
    if command == "login":
        username, password = sys.argv[4], os.environ["CUTOVER_PASSWORD"]
        secret, _ = c.secret(username, password)
        status, body = c.call("POST", "/api/auth/login", {"username": username, "authSecret": secret})
        assert status == 200, f"login: {status} {body.get('error', '')}"
    elif command == "change-password":
        username, old, new = sys.argv[4], os.environ["CUTOVER_OLD_PASSWORD"], os.environ["CUTOVER_PASSWORD"]
        old_secret, params = c.secret(username, old)
        salt = base64.b64encode(os.urandom(16)).decode()
        status, body = c.call("POST", "/api/auth/password", {
            "oldAuthSecret": old_secret, "newAuthSecret": derive(new, salt, params["iterations"]),
            "newLoginSalt": salt, "newIterations": params["iterations"]})
        assert status == 200, f"change password: {status} {body.get('error', '')}"
    elif command == "pin-key":
        username, password, key_file = sys.argv[4], os.environ["CUTOVER_PASSWORD"], sys.argv[5]
        secret, _ = c.secret(username, password)
        key = open(key_file).read().strip()
        status, body = c.call("POST", "/api/admin/backup/pin-key", {"authSecret": secret, "publicKey": key, "threshold": 2, "totalShares": 3})
        assert status == 200, f"pin-key: {status} {body.get('error', '')}"
    elif command == "backup-now":
        username, password = sys.argv[4], os.environ["CUTOVER_PASSWORD"]
        secret, _ = c.secret(username, password)
        status, body = c.call("POST", "/api/admin/backup/run", {"authSecret": secret})
        assert status == 200, f"backup run: {status} {body.get('error', '')}"
    elif command == "signed-in":
        status, body = c.call("GET", "/api/auth/me")
        assert status == 200 and body.get("authenticated") is True, f"/api/auth/me: {status} authenticated={body.get('authenticated')}"
    elif command == "session-cookie":
        # For the script to replay against each replica directly; a throwaway test session.
        print(next(c.value for c in c.jar if c.name == "kypost_session"))
    elif command == "key-id":
        status, body = c.call("GET", "/api/admin/backup/status")
        assert status == 200, f"backup status: {status}"
        print(body.get("keyId", ""))
    else:
        sys.exit(f"unknown command {command}")


if __name__ == "__main__":
    main()
