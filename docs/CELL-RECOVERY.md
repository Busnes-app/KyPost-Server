# Cells: region codes and regional disaster recovery

Operator runbook for a hosted, multi-tenant KyPost deployment that serves one or more
KyIdentity cells (`KY_CELLS`). The design is `docs/superpowers/specs/2026-11-22-multi-host-replicas-design.md`
("Regions", "Failover and split brain between regions", "MH9 region codes and recovery as built").

**Status.** Multi-tenant startup is refused in every release build (tenancy design, "The
hosted launch gate"). The PostgreSQL steps below were rehearsed on disposable containers; the
steps that start KyPost in multi mode cannot run in a release build and are marked *unproven*. Self-hosted installs have
no cells and need nothing here: their recovery is [RESTORE.md](RESTORE.md).

## What a cell is

- A cell is one KyIdentity `home_region` slug, such as `us-east-2`. A deployment serves the cells
  in `KY_CELLS` and refuses every org of another cell
  ([CONFIGURATION.md](CONFIGURATION.md)).
- Each cell keeps its data in one PostgreSQL primary with its own replicas, receiving servers,
  keys and backups. An org never changes cell.
- The cell is logical. Moving it to another data centre keeps its slug, its region code, its
  keys and its pins.

## Region codes

A masked address on the hosted relay domain carries its cell: the character right after
`adjective.animal.` is the cell's **region code**, one lowercase base32 character (`a`–`z`,
`2`–`7`). There is one relay domain for every cell. The MX helper reads the code to find the
cell (MH10).

The table is `relayCodes` in `backend/internal/tenancy/relaycodes.go`.

| Rule | Why |
|---|---|
| One code per cell, one cell per code, at most 32 | The code is a single base32 character |
| Append only: never change, remove or reuse an entry | Every address allocated under a code keeps it |
| A closed cell keeps its entry | Its addresses still exist, and a reused code would deliver them to another cell |
| A cell with no code cannot start | A multi start refuses: `KY_CELLS names a cell with no masked relay region code: <cell>` |

**No cell has a code yet.** The owner names the launch cells and their codes. Until then every
multi start refuses at this check, test builds included.

To open a cell:

1. Choose the next unused code. Never reuse one, even a closed cell's.
2. Add `{code, cell}` to `relayCodes` and the same entry to `releasedRelayCodes` in
   `relaycodes_test.go`, in one reviewed PR. `TestRelayCodeTableIsStableAndUnique` fails if an
   earlier entry changed. `TestRelayCodeTableRefuses` covers the rules.
3. Release, then start the cell with `KY_CELLS` naming it.

## Regional disaster recovery

This covers losing the region a cell runs in. Inside one region, the database service's
synchronous standby in another zone covers a lost host or zone, and nothing here is needed.

Two ways back:

- **Capsule restore** (the floor). Restore the cell's latest capsule into a new primary in another
  region of the same residency zone ([RESTORE.md](RESTORE.md)). You lose everything since that
  capsule.
- **Promote an asynchronous standby** in another region of the same residency zone. You lose the
  replication lag, so mail that was answered `250` inside that window can be lost. That makes
  this a **declared disaster with a stated loss**, never a routine switch.

Advisory locks are not replicated, so nothing in KyPost can detect two primaries. **Fence first.**
Do the steps in order.

### 1. Declare, and record the loss window

On the standby (rehearsed):

```sh
psql "$STANDBY_URL" -Atc "SELECT pg_is_in_recovery(), now() - pg_last_xact_replay_timestamp()"
```

`t` and an interval: the standby is still a standby, and the interval is roughly how much
committed work it lacks. Write it in the incident record before you promote.

### 2. Fence the old cell

Stop everything that could still write to the old primary or answer for the cell:

```sh
# On each old replica host, if reachable (rehearsed):
docker compose stop kypost-server receiving-a receiving-b
```

Then stop the old primary, or cut its network at the provider if the host is unreachable. The
old MX records must not reach the old receiving servers.

Do not continue until the old primary can take no connection. A replica or receiving server
left running against it after the promotion is split brain: two primaries accepting mail for
the same orgs.

### 3. Promote the standby

```sh
psql "$STANDBY_URL" -Atc "SELECT pg_promote(wait => true)"   # t
psql "$STANDBY_URL" -Atc "SELECT pg_is_in_recovery()"        # f
```

Both rehearsed. A managed database service has its own promote action; use it, then check
`pg_is_in_recovery()` the same way.

### 4. Check that the promoted database is the cell's

```sh
psql "$NEW_URL" -Atc "SELECT key FROM kypost_state.meta WHERE key IN ('tenancy_mode', 'staff_org_id', 'replica_config_fingerprint') ORDER BY key"
```

All three keys must be present (rehearsed with a single-tenant fixture, which has only
`replica_config_fingerprint`). The fingerprint holds HMACs, never values. Do not copy values out.

### 5. Move the hostname and the MX records

- Point the cell's public hostname (each cell has its own) at the new region's proxy.
- Point the MX records of the cell's primary domains at the new region's MX hosts.
- On the relay domain, the MX hosts of every region forward by region code (MH10). Update each
  MX host's `KYPOST_RECEIVING_REMOTE` file with the new receiving servers' private addresses.

Check from outside:

```sh
dig +short MX example-cell-domain.test
dig +short mail.us-east-2.example.test
```

### 6. Start the cell in the new region (*unproven: release builds refuse multi*)

Start the replicas and receiving servers with:

- **The same settings**: `KYPOST_TENANCY=multi`, the same `KYPOST_STAFF_ORG_ID` and `KY_CELLS`,
  and every other shared setting.
- **The same keys**: the same `SECRET_DIR` keys (MH5's provisioned keys and
  `receiving-tls.key`, so the MX hosts' pins of the receiving servers stay valid).
- **The new primary**: `KYPOST_DATABASE_URL` points at it with `sslmode=verify-full`.

The gateway pins live in `kypost_tenancy`, so the MX hosts stay enrolled.

A replica started with other shared settings, another key or another cell list refuses to start
and names the setting (rehearsed in single mode: another `totp-secret.key` is refused against the
promoted database). Fix the replica, not the record. Run `accept-replica-config` only for a
change you meant to make.

### 7. After

- Keep the old primary fenced. Never fail back by starting it: rebuild it as a new standby of the
  new primary with `pg_basebackup` (rehearsed), then remove the fence.
- Rebuild an asynchronous standby in another region, and take a capsule.
- Mail senders retry during the outage, so most inbound mail arrives late rather than lost.

## Rehearsal

`scripts/rehearse-cell-promotion.sh` runs steps 1–4 on disposable containers and checks the
step 6 refusal in single mode:

1. A PostgreSQL 18 primary and a streaming standby, made with `pg_basebackup -R`.
2. A KyPost server writes its record and is stopped.
3. The primary is fenced, the standby promoted, and its records checked.
4. KyPost starts against the promoted database with the same keys, and is refused with another
   key.
5. The old primary is rebuilt as a standby of the new one.

It needs Docker and a KyPost image, touches nothing else, and removes everything it made on exit.
