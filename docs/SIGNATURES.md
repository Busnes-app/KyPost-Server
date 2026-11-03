# Email signatures

Each account keeps its email signatures on the server, so every KyPost client
inserts the same ones. This file is the contract the web app and the native
clients (Android, Mac, Linux) implement against.

## Storage and API

`GET /api/mail/signatures` (web session or a paired device's credentials) and
`PUT /api/mail/signatures` (web session only, CSRF-protected, like the other
settings writes) read and replace the whole document:

```json
{
  "signatures": [{ "id": "a1b2", "name": "Work", "text": "Ada Lovelace\nAnalyst" }],
  "defaultId": "a1b2",
  "byAddress": { "ada@example.org": "a1b2", "alias@example.org": "" },
  "onReply": true,
  "onForward": true
}
```

- `text` is **plain text**. It is stored and served verbatim and is never HTML.
  A client must render it as text: escape it into its editor, never parse it as
  markup. There is no HTML signature field, so the server never holds markup a
  client could execute.
- Limits, refused with `400 {"error"}`: at most 10 signatures; `id` 1–64 of
  `A-Z a-z 0-9 - _`, unique (the client creates it); `name` required, at most 60
  characters, no control characters; `text` at most 2000 characters, no control
  characters except newline and tab, and no bidirectional override or isolate
  controls. `\r\n` is stored as `\n`. Unknown fields are refused.
- `defaultId` is `""` or an existing id. `byAddress` has at most 100 entries;
  keys are bare addresses, stored lowercase; values are an existing id or `""`.
- A new account answers no signatures, `defaultId` `""`, and `onReply` and
  `onForward` true. The document lives in the account's `config.yaml` and is in
  every backup that holds the config directory.

## Choosing and inserting

For a message sent from address `from` (the account address when the client
has no explicit From):

1. Replying and `onReply` is false, or forwarding and `onForward` is false: no
   signature.
2. `byAddress[lowercase(from)]` exists: that signature, where `""` means none
   (it does not fall back to the default).
3. Otherwise `defaultId`, where `""` means none.

Insert it once, when the compose window opens: below the line the user types
on, and above any quoted message, with one blank line before it and one
between it and the quote. Do not add a `-- ` delimiter the user did not write. Opening a saved draft or restoring an
autosaved one inserts nothing; its body already holds whatever the author
left. When the user changes From, replace the inserted signature with the new
address's, unless they have edited it, in which case leave it alone. A window
holding only its signature counts as empty for autosave and discard prompts.
If the signatures cannot be fetched, compose without one.
