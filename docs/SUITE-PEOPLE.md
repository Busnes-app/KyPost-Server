# Organisation people in recipient autocomplete

Section D of the wave 1 suite integrations contract
(`ky-redesign/integrations/suite-integrations-contracts.md`), KyPost part, copied
here on 2026-11-14; this file is now the durable copy.

## KyIdentity

`GET {issuer}/api/suite/people?sub=&q=&limit=` (KyIdentity `docs/SUITE-DIRECTORY.md`,
client `ky-primitives/suitedir` `People`), with KyPost's own SSO client credentials.
`q` is 2 to 64 characters, matched case-insensitively on display name, username or
email; `limit` 1 to 20. The result is active users who may open KyPost, dedicated
administrator identities excluded, as `{"people":[{"sub","name","email"}]}`. A `sub`
that is not active or may not open KyPost gets an empty list.

## KyPost route

`GET /api/suite/people?q=` (session, CSRF rules as other GET routes, `no-store`):

- Asks KyIdentity with the session's linked subject, limit 10, and answers
  `{"people":[{"name","email"}]}` (no subject reaches the browser).
- Cached per user, subject and lowercased query for 60 seconds (at most 2000 entries,
  cleared whole when full); an abandoned request is not cached.
- Always 200 with a list, empty for a query under 2 or over 64 characters, an account
  not linked to the configured issuer, SSO off, or any KyIdentity failure.
- Per-user rate limit (burst 30, two a second): 429 `rate_limited` with `Retry-After`.
- Logs carry the user ID and suitedir's error only, never the query.

## Web client

Recipient autocomplete (`hooks/useContactAutocomplete.ts`) lists the user's own
contacts first, then organisation people under one "Organisation" heading,
deduplicated by address (case-insensitive; a contact wins). Organisation search runs
from two characters; its failure leaves the contacts. Free text still works.
