# KyPost Calendar & CalDAV Specification
**Document:** Working Specification & Implementation Blueprint  
**Component:** `kypost-server` (Backend & Webmail)  
**Status:** Draft / Ready for Implementation  
**Author:** Busnes.app Architecture Team  
**Date:** September 2026  

---

## 1. Executive Summary & Architectural Motivation

KyPost is currently a self-hosted IMAP/SMTP webmail suite with contact management and a native **CardDAV** server mounted at `/dav`. 

This document defines the architecture, data models, wire protocols, and execution plan for adding **Calendar & Scheduling (CalDAV / iCalendar)** directly into KyPost.

### Why Calendar Belongs in KyPost (and not a standalone server)
1. **Shared Protocol Foundation:** KyPost already relies on `github.com/emersion/go-webdav` for CardDAV. The same library provides native `caldav.Handler` and `caldav.Backend` implementations with minimal overhead.
2. **Unified Account Provisioning:** Native clients (macOS, iOS, Linux Evolution/Thunderbird, Android via DAVx5) treat Mail, Contacts, and Calendars as a single **Groupware Account**. Serving both CardDAV and CalDAV under the unified `/dav` tree allows a single app-password and discovery URL to configure an entire personal information management (PIM) suite.
3. **iMIP / iTIP Email Interoperability (RFC 6047 / RFC 5546):** Calendar invitations (`VEVENT`) arrive as `text/calendar` email attachments. Managing calendars inside KyPost enables zero-friction inline RSVP actions ("Accept", "Tentative", "Decline") that automatically update local calendar state and transmit signed, DKIM-verified reply emails.
4. **Shared Security & Recovery Boundary:** Calendars share the same user authentication, app passwords (`dav_auth.go`), rate-limiting, and blind Shamir custodian backups ([KyRecovery](file:///home/yoshi/git/busnes.app/kyrecovery-server)) as mail and contacts.

---

## 2. Standards & RFC Compliance

The calendar implementation must adhere strictly to these open standards:

| Standard | Title | Role in KyPost |
| :--- | :--- | :--- |
| **RFC 4791** | Calendaring Extensions to WebDAV (CalDAV) | Core protocol for calendar collection discovery, event queries, and multiget. |
| **RFC 5545** | Internet Calendaring and Scheduling (iCalendar) | Data format for `VEVENT`, `VTODO`, `VJOURNAL`, `VALARM`, and `RRULE`. |
| **RFC 6638** | Scheduling Extensions to CalDAV | Inbox/Outbox collections for calendar-based user-to-user scheduling. |
| **RFC 6047** | iCalendar Message-Based Interoperability (iMIP) | Email-based meeting invitations, cancellations, and status updates over SMTP. |
| **RFC 6578** | Collection Synchronization for WebDAV | `sync-collection` REPORT support for efficient differential syncing on mobile. |
| **RFC 6764** | Locating Services (CalDAV/CardDAV Discovery) | `/.well-known/caldav` redirection. |

---

## 3. URL Namespace & Discovery Topology

CalDAV resources are mounted within the existing `/dav` prefix:

```
https://<kypost-domain>/
├── /.well-known/caldav                          -> HTTP 301/308 redirect to /dav/
├── /dav/                                        -> Root WebDAV principal discovery
│   └── principals/
│       └── users/
│           └── {username}/                      -> Current user principal
│               ├── calendar-home-set/           -> Points to /dav/{username}/calendars/
│               └── addressbook-home-set/        -> Points to /dav/{username}/contacts/ (Existing)
└── /dav/{username}/
    ├── calendars/
    │   ├── default/                             -> Primary Personal Calendar collection
    │   │   ├── {event-uuid}.ics                 -> Individual calendar object
    │   │   └── ...
    │   ├── work/                                -> Secondary Calendar collection
    │   ├── inbox/                               -> CalDAV Scheduling Inbox (RFC 6638)
    │   └── outbox/                              -> CalDAV Scheduling Outbox (RFC 6638)
    └── contacts/                                -> CardDAV address books (Existing)
```

---

## 4. Backend Implementation Plan

### 4.1. Library Dependencies
KyPost's `backend/go.mod` already includes `github.com/emersion/go-webdav`. The calendar subsystem will introduce:
* `github.com/emersion/go-webdav/caldav`: Protocol handlers, XML marshal/unmarshal for `calendar-query` and `calendar-multiget`.
* `github.com/emersion/go-ical`: High-performance iCalendar component parser and serializer.

### 4.2. Database Schema (SQLite / WAL)
The calendar schema will be created in the existing per-user or shared application SQLite database (`internal/store`):

```sql
-- Calendar collections owned by a user
CREATE TABLE calendars (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug TEXT NOT NULL,                     -- e.g. "default", "work"
    display_name TEXT NOT NULL,
    description TEXT DEFAULT '',
    color TEXT DEFAULT '#bf3f18',           -- Hex swatch (default: Busnes terracotta)
    timezone TEXT DEFAULT 'UTC',
    order_index INTEGER DEFAULT 0,
    ctag TEXT NOT NULL,                     -- Collection tag (updated on any item edit)
    sync_token TEXT NOT NULL,               -- RFC 6578 sync token
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, slug)
);

-- Individual calendar objects (VEVENT, VTODO)
CREATE TABLE calendar_objects (
    id TEXT PRIMARY KEY,                    -- UUID
    calendar_id TEXT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    uid TEXT NOT NULL,                      -- RFC 5545 UID header
    path TEXT NOT NULL,                     -- e.g. "meeting-123.ics"
    etag TEXT NOT NULL,                     -- SHA-256 digest of ical_data
    summary TEXT NOT NULL DEFAULT '',
    description TEXT DEFAULT '',
    location TEXT DEFAULT '',
    dtstart TIMESTAMP,                      -- Indexed for range queries
    dtend TIMESTAMP,                        -- Indexed for range queries
    all_day BOOLEAN DEFAULT FALSE,
    rrule TEXT DEFAULT NULL,                -- Recurrence rule if recurring
    status TEXT DEFAULT 'CONFIRMED',        -- CONFIRMED, TENTATIVE, CANCELLED
    transp TEXT DEFAULT 'OPAQUE',           -- OPAQUE (busy) or TRANSPARENT (free)
    ical_data BLOB NOT NULL,                -- Full raw RFC 5545 iCalendar payload
    size_bytes INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(calendar_id, path),
    UNIQUE(calendar_id, uid)
);

-- Indices for high-speed CalDAV calendar-query time-range filtering
CREATE INDEX idx_calendar_objects_timerange ON calendar_objects(calendar_id, dtstart, dtend);
CREATE INDEX idx_calendar_objects_uid ON calendar_objects(uid);
```

### 4.3. CalDAV Backend Interface Implementation
Implement `caldav.Backend` (`internal/calendar/backend.go`):
* `CalendarHomeSetPath(ctx, user)`: Returns `/dav/{username}/calendars/`.
* `ListCalendars(ctx)`: Returns all active calendars for the authenticated user.
* `GetCalendar(ctx, path)`: Retrieves collection properties (`displayname`, `calendar-color`, `supported-calendar-component-set`, `ctag`, `sync-token`).
* `CreateCalendar(ctx, path, opts)`: Creates secondary calendars.
* `DeleteCalendar(ctx, path)`: Deletes secondary calendars.
* `GetCalendarObject(ctx, path)`: Fetches an individual `.ics` object by URL.
* `ListCalendarObjects(ctx, path)`: Returns objects (supports `calendar-multiget` and `calendar-query` time-range evaluation).
* `PutCalendarObject(ctx, path, cal, opts)`: Validates, parses, indexes, and stores uploaded `.ics` bodies. Enforces max payload bounds (e.g. 10MB).
* `DeleteCalendarObject(ctx, path)`: Removes an event and updates collection `ctag`.

### 4.4. Security, Concurrency & DoS Guards
* **Auth:** Reuses `withDAVBasicAuth` from `backend/internal/api/dav_auth.go` using user app-passwords.
* **Path Traversal / Cross-User Rejection:** Path segments under `/dav/` must strictly match the authenticated user's scope.
* **Multistatus Concurrency Slots:** Reuses `acquireDAVMultistatusSlot` to prevent runaway memory allocation during broad sync requests.
* **Decompression Bomb Protection:** Restricts incoming PUT/REPORT payloads using `http.MaxBytesReader`.

---

## 5. Webmail & Email Interlocking (iMIP / RFC 6047)

A calendar in KyPost must seamlessly handle meeting invites arriving via email.

```
[ Incoming Email with text/calendar ]
                 │
                 ▼
[ MIME Inspection & ical Parser ]
                 │
                 ├── Detected: METHOD:REQUEST (Meeting Invitation)
                 │
                 ▼
┌─────────────────────────────────────────────────────────────┐
│                 WEBMAIL INBOX ACTION BAR                    │
│                                                             │
│  📅 Product Architecture Sync                               │
│  When: Thursday, Oct 2, 2026, 14:00 – 15:00 UTC            │
│  Organizer: alex@partner.com                                │
│                                                             │
│  [ Accept ]        [ Tentative ]        [ Decline ]         │
└──────────────────────────────┬──────────────────────────────┘
                               │
               User clicks [ Accept ]
                               │
        ┌──────────────────────┴──────────────────────┐
        ▼                                             ▼
[ Local Calendar DB ]                         [ Outgoing Reply ]
  • Inserts or updates event                    • Generates METHOD:REPLY .ics
  • Sets PARTSTAT=ACCEPTED                      • Signs with user's OpenPGP key
  • Marks calendar collection dirty             • Sends via user's SMTP alias
```

### Flow Specifications:
1. **Parser Hook:** During message rendering, KyPost scans MIME attachments for `text/calendar` or `application/ics`.
2. **Status Matching:** Queries the database by `UID`. If the meeting already exists, displays current RSVP status ("You accepted this invitation").
3. **Response Dispatcher (`internal/calendar/imip.go`):**
   * Creates an RFC 5546 `METHOD:REPLY` payload with updated `ATTENDEE;PARTSTAT=ACCEPTED|DECLINED|TENTATIVE`.
   * Sends an email via the user's outbound SMTP configuration to the `ORGANIZER`.
   * If OpenPGP signing is enabled for the account, the `.ics` reply is signed using the user's browser-unlocked PGP key.

---

## 6. Frontend UI: KyPost Calendar Module

The calendar interface will live inside the existing React/TypeScript frontend (`frontend/src/`):

### 6.1. Navigation & Views
* Added to the main sidebar navigation (alongside **Mail**, **Contacts**, and **Settings**).
* **View Modes:**
  * **Month View:** High-level grid with overflow indicators.
  * **Week View:** 7-day time grid with drag-to-create.
  * **Day View:** Detailed hour schedule.
  * **Agenda View:** Chronological list of upcoming events.

### 6.2. Visual Styling & Tokens
* Strictly follows [ky-ui](file:///home/yoshi/git/busnes.app/ky-ui) design tokens:
  * Light Background: `#f8f6f0` (Warm Cream)
  * Dark Background: `#182326` (Deep Charcoal)
  * Accents: Busnes terracotta (`#bf3f18` / `#f5865f`)
  * Typography: Space Grotesk (headers) and IBM Plex Mono (timestamps/dates).

### 6.3. Event Editor Modal
* **Title, Location, Description.**
* **Start & End Time:** Date pickers, time dropdowns, "All Day" toggle.
* **Recurrence (RRULE):** None, Daily, Weekly, Monthly, Yearly, Custom (interval, days of week, end date).
* **Reminders (VALARM):** Configured via browser push notifications.
* **Attendee Management:** Autocomplete integrated with the existing KyPost Contact Address Book (`GET /api/contacts/autocomplete`).

---

## 7. Backup, Drills & KyRecovery Integration

Following the Ky Suite contract defined in [AGENTS.md](file:///home/yoshi/git/busnes.app/AGENTS.md):

1. **Backup Capsule Expansion:**
   * KyPost's `internal/backup` runner is updated to export calendar tables (`calendars`, `calendar_objects`) as an atomic snapshot.
   * Serialized into the `.kycap/3` backup archive under `data/calendars.sqlite3`.
2. **Blind Deposit:**
   * Uploaded to [KyRecovery](file:///home/yoshi/git/busnes.app/kyrecovery-server) via `POST /api/backup/deposit`.
3. **Restore Drill (`RESTORE.md`):**
   * The offline restore tool `kypost restore --capsule <path>` verifies that restored calendar objects match the original SHA-256 digests and reconstructs calendar collections cleanly.

---

## 8. Implementation Phases & Verification Checklist

### Phase 1: Core CalDAV Engine (Backend)
- [ ] Add `github.com/emersion/go-ical` to `backend/go.mod`.
- [ ] Implement SQLite schema migrations for `calendars` and `calendar_objects`.
- [ ] Build `internal/calendar/backend.go` satisfying `caldav.Backend`.
- [ ] Mount `caldav.Handler` under `/dav/` and configure `/.well-known/caldav` redirection.
- [ ] Unit & integration tests: verify RFC 4791 `PROPFIND`, `REPORT (calendar-query)`, `PUT`, `DELETE`.

### Phase 2: Client Compatibility Testing
- [ ] Test sync against Apple Calendar (macOS & iOS).
- [ ] Test sync against Thunderbird / Evolution (Linux).
- [ ] Test sync against DAVx5 (Android).
- [ ] Validate RFC 6578 sync-tokens (differential sync efficiency).

### Phase 3: iMIP / Email Integration
- [ ] Build MIME parser for `text/calendar` attachments in incoming mail.
- [ ] Implement Webmail RSVP component.
- [ ] Implement SMTP `METHOD:REPLY` generator with optional PGP signing.

### Phase 4: Frontend Calendar UI
- [ ] Implement Calendar navigation tab in `frontend/src`.
- [ ] Build Month / Week / Day / Agenda views using `ky-ui` tokens.
- [ ] Implement Event Creation & Edit modal with contact attendee autocomplete.
- [ ] Connect web view to `/api/calendars` REST endpoints.

### Phase 5: Backup & Verification Pass
- [ ] Wire calendar state into `internal/backup/schedule.go`.
- [ ] Execute `make test` across the full KyPost backend test suite.
- [ ] Run offline recovery drill verifying zero data loss.
