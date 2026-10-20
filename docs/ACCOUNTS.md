# Accounts

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Session Behavior

- Login sessions expire after 24 hours without activity.
- Session expiry slides forward. Each authenticated request extends the TTL by
  24 hours.
- A session also has a hard cap. It dies 7 days after KyPost issued it, whatever
  the activity. A thief cannot keep a stolen cookie alive with their own traffic.
- KyPost sweeps expired sessions every hour. It does not wait for the cookie to
  arrive again.
- Logout invalidates the server-side session and clears the cookie.
- A deactivation or a role change takes effect on the user's next request, not
  at the next login.

## Users and Roles

Accounts live in `/kypost/config/users.json`. The roles are `admin` and `user`.

- Admins manage users from Settings, under Admin. The Server panel creates
  users, changes roles, resets passwords, and deactivates or reactivates
  accounts, alongside runtime settings, updates, verified mail domains and native addresses.
  Diagnostics holds the full health view, the system logs and health repair.
  Default Labels is a Server tab: the label list new accounts start from.
  Email Labels is entirely per-user — each account's own prompt tuning and
  classification decisions — so it is not admin-only.
- Users connect their own IMAP and SMTP account. They read and label their own
  mail, pair their own devices, set their own notification preferences, and tune
  their own prompt.
- Admins can also do the connecting. **Server > Default Mail Server** publishes
  host and port defaults that prefill every new user's Email Settings form; users
  still supply their own username and password. **Server > Manage Users > Mailbox**
  writes a specific user's IMAP/SMTP and CardDAV-client credentials directly, with
  an optional lock. A locked user still sees the host and username on their
  Settings > Mail tabs, rendered read-only with a "managed by your administrator"
  notice, and the server refuses their own writes. Send-As addresses, Mailbox
  Rules, and the built-in CardDAV Access password are never admin-managed. Every
  admin assignment is logged with `user_id`, `admin_id`, and `managed`.
- Deactivation is a soft delete. The user can no longer sign in. KyPost keeps
  their data on disk until you remove it manually.
- KyPost does not let you deactivate or demote the last active admin.

Per-user data layout:

- `/kypost/config/users/<userID>/`: encrypted IMAP credentials, `carddav-client.json` (encrypted CardDAV-client credentials), tuning prompt (`tuning.md`), notification preferences (`config.yaml`)
- `/kypost/state/users/<userID>/`: `state.db` — an SQLite database holding the mailbox checkpoint, the processed set, decision history, push subscriptions, and paired devices. SQLite runs in WAL mode, so `state.db-wal` and `state.db-shm` sit alongside it while the database is open and are part of the state, not scratch files.

Upgrade from a single-admin installation: on the first start, KyPost imports the
legacy `admin.env` account into `users.json`. KyPost also copies the legacy
global mailbox state, IMAP credentials, tuning file, and notification
preferences into that admin's per-user directories. KyPost leaves the legacy
files in place but no longer reads them. There is no automatic rollback. To
reset to a fresh multi-user state, delete `users.json` and the `users/`
directories.
