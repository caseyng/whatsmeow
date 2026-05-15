# WhatsApp Integration — Implementation Experience

This document captures the reasoning behind every significant decision made while building the
whatsmeow fork and the test-connect CLI. It exists because the same problems will reappear
during Android app development. The code shows *what* was done; this explains *why*, what
was tried first, and what traps to avoid.

---

## Architecture Decisions

### Single shared SQLite connection

**Decision:** `sqlstore.NewWithDB(rawDB, ...)` shares the same `*sql.DB` between whatsmeow's
internal tables and our `wa_messages`/`wa_chats` tables.

**Why:** whatsmeow writes internally (push names, session keys, app state) concurrently with
our history sync batch inserts. Two separate `sql.Open` calls on the same file created a second
WAL writer that conflicted with the first, producing `database is locked` errors during heavy
history sync. One connection, one writer.

**DSN that must be used:**
```
file:<path>?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000
```
- `WAL`: allows concurrent reads while writing — Android Room can read the DB while Go writes
- `_busy_timeout=5000`: SQLite retries for up to 5s before returning "locked"; prevents failures
  during the burst of writes that accompany a history sync
- `_foreign_keys=on`: belt-and-suspenders; our schema doesn't enforce FKs but good practice

**Wrong instinct:** "just open a separate DB file for messages." This works for reads but any
write contention with whatsmeow's internal writes still hits the shared WAL on the same file if
whatsmeow has a connection to it.

---

### Chrome HTTP client (uTLS) — two separate hooks

**Decision:** Wrap both `SetWebsocketHTTPClient` and `SetPreLoginHTTPClient` with a custom
HTTP client that impersonates Chrome's TLS fingerprint via `github.com/refraction-networking/utls`.

**Why:** Go's stdlib TLS produces a JA3/JA4 fingerprint that is detectably non-browser. WhatsApp
can (and does) fingerprint TLS to identify non-browser clients. The fork paper (arXiv:2504.07323,
"Prekey Pogo") documents that fingerprinting happens at multiple protocol layers.

**Critical ALPN bug:** `HelloChrome_Auto` includes `h2` in ALPN because Chrome does. But
Go's `http.Transport` with a custom `DialTLSContext` cannot speak HTTP/2. So the WebSocket
upgrade negotiated h2 and the connection broke with: `malformed HTTP status code "\x00\x00\x00..."`.

**Fix:** After `BuildHandshakeState()`, find the `ALPNExtension` in `uconn.Extensions` and
overwrite its `AlpnProtocols` field with `["http/1.1"]` before calling `HandshakeContext`.

**Wrong instinct:** patch `HandshakeState.Hello.AlpnProtocols` directly. This is overwritten
by `ApplyConfig()` which re-runs every extension's `writeToUConn` during handshake. You must
patch the extension *object*, not the Hello struct.

**Why two hooks:** `SetWebsocketHTTPClient` covers the WebSocket upgrade after login;
`SetPreLoginHTTPClient` covers the initial HTTPS requests that happen before any session exists.
Missing either one leaves the pre-login or post-login path with a Go stdlib fingerprint.

---

### Phone mismatch guard

**Decision:** After `container.GetFirstDevice()`, check `device.ID.User != phone` and exit with
an error if mismatched.

**Why:** `GetFirstDevice()` returns *any* stored session, not the one for the phone number you
passed. If a user runs `test-connect 6591234567` while the DB already holds a session for
`6599887766`, whatsmeow will happily reconnect as 6599887766. The wrong session gets used
silently. One DB file per account; enforce it explicitly.

---

### INSERT OR IGNORE for message storage

**Decision:** `INSERT OR IGNORE INTO wa_messages` with message ID as primary key.

**Why:** History sync sends the same messages again on every re-pair. Using `INSERT OR IGNORE`
makes the entire sync operation idempotent — re-syncing after unpairing and re-pairing doesn't
create duplicates and doesn't fail. The `storeMessage` return value (true = inserted, false =
already existed) lets us count only newly stored messages.

---

### upsertChat before wantChat filter

**Decision:** Call `upsertChat` for every conversation in a history sync event, then apply
the `--chat` filter only to message storage.

**Why:** Chat names and metadata (description, participants, archived state) are only available
during history sync. If you filter at the conversation level, you lose the name of every chat
you're not storing messages for. The app will need to display chat names regardless of whether
messages are stored.

---

### ON_DEMAND sync bypasses --chat filter

**Decision:** When the HistorySync sync type is `ON_DEMAND`, store all messages regardless of
the `--chat` flag.

**Why:** An ON_DEMAND sync is a response to a specific request we made (`BuildHistorySyncRequest`).
We explicitly asked for that chat. Applying the filter to the response means we'd silently discard
exactly the messages we requested. The filter exists for the passive initial sync; it should not
block the active backfill path.

---

### `returned` vs `stored` for backfill termination

**Decision:** In the ON_DEMAND handler, count total messages *returned* in the response
separately from messages *stored* (newly inserted). Use `returned == 0` to detect end of history.

**Why:** `stored == 0` can happen when all returned messages are already in the DB (e.g. resuming
a partial backfill). If you stop on `stored == 0`, you terminate prematurely when the DB is
partially filled. `returned == 0` means WhatsApp sent back an empty batch — it has no more
history to give, which is the true termination condition.

---

### Reaction storage: DELETE on empty emoji

**Decision:** `storeReaction` with an empty emoji string → `DELETE` the row; non-empty → upsert.

**Why:** WhatsApp sends an unreaction as a reaction message with `text = ""`. Storing a row with
an empty emoji would require callers to filter it out everywhere. Delete on unreaction keeps the
table clean and semantically correct.

---

### is_deleted flag vs row deletion for revoked messages

**Decision:** When `ProtocolMessage_REVOKE` arrives (or `RevokeMessageTimestamp > 0` in history
sync), set `is_deleted = 1` on the existing row rather than deleting it.

**Why:** The app may want to show "this message was deleted" in the UI, which requires knowing
a message existed. Hard-deleting the row loses that information. The `is_deleted` flag preserves
the fact of existence while signalling that content is gone.

---

## WhatsApp Protocol Behaviour (Empirical)

### History sync stages

WhatsApp delivers history to a new linked device in this sequence:

```
INITIAL_BOOTSTRAP  → bulk of recent conversations and messages
PUSH_NAME          → display names for contacts
INITIAL_STATUS_V3  → status/stories
NON_BLOCKING_DATA  → additional metadata
RECENT             → final batch; signals sync is complete
```

`RECENT` is the terminal batch. If you want to auto-disconnect after sync, the timer should
fire after `RECENT` is received (or after 30s of no new sync events as a practical proxy).
`FULL` only appears when Transfer Chat History is triggered at pairing time.

---

### The two-tier history: rolling window vs stub

WhatsApp does **not** send a uniform history window to linked devices. It applies two
different strategies based on chat activity:

**Active chats (frequently messaged):**
Receive a rolling window of recent messages. In practice this was ~25–30 days for this account.
The window is server-controlled and not configurable from the client.

**Inactive chats (infrequent or dormant):**
Receive exactly **1 message** — the most recent one, no matter how old. This is a stub: it
makes the chat appear in the list with a preview, but provides no real history. Stubs can be
years old. There is no cutoff date; the rule is behavioural, not temporal.

**Implication for the app:** Never assume message count reflects conversation depth. A chat
with 1 stored message might have years of history accessible via on-demand backfill. The app
should offer a "load more" / scroll-up mechanism that triggers `BuildHistorySyncRequest`.

---

### On-demand backfill (scroll-up equivalent)

WhatsApp Web loads older messages when you scroll up by sending a peer message to the primary
phone. whatsmeow exposes this as:

```go
req := client.BuildHistorySyncRequest(oldestKnownMessageInfo, count)
client.SendPeerMessage(ctx, req)
// Response arrives as *events.HistorySync with SyncType == ON_DEMAND
```

Key behaviours:
- Response comes from the **primary phone**, not WhatsApp servers — primary must be online
- `count` is capped by WhatsApp (50 is the recommended batch size)
- Repeat until the response returns 0 messages
- Progress is incremental: safe to interrupt and resume; re-requests overlap is handled by
  INSERT OR IGNORE

---

### Full history transfer

Only available via Settings → Chats → Chat History → Transfer Chat History on the primary
phone, triggered **at or immediately after pairing**. Delivers sync type `FULL`. This is a
local proximity transfer over WiFi, not through WhatsApp servers. Once the linked device has
been connected for a while, the option disappears. Cannot be triggered programmatically.

---

### JID formats

| Suffix | Meaning |
|---|---|
| `@s.whatsapp.net` | Individual contact |
| `@g.us` | Group chat |
| `@lid` | Internal LID addressing (WhatsApp-internal alias, treat same as individual) |
| `0@s.whatsapp.net` | System/WhatsApp-generated messages |

Group membership is indicated by `@g.us` suffix on the chat JID, not by any flag. Check with
`strings.HasSuffix(chatJID, "@g.us")`.

---

### Message status enum

`WebMessageInfo_Status` in history sync:

| Value | Meaning |
|---|---|
| ERROR | Failed to send |
| PENDING | Queued, not yet sent |
| SERVER_ACK | Reached WhatsApp servers |
| DELIVERY_ACK | Delivered to recipient device |
| READ | Read by recipient |
| PLAYED | Played (audio/video) |

Live incoming messages don't carry a useful status (you received them; the status is theirs to
track). Status is most meaningful for your own sent messages in history sync.

---

### Group participant ranks

`GroupParticipant_Rank`: `REGULAR`, `ADMIN`, `SUPERADMIN`. Stored as strings. Available in
history sync `conv.GetParticipant()`. Not updated live — only refreshed when a new history sync
arrives or the group info is explicitly fetched via `GetGroupInfo`.

---

## Security & Fingerprinting

WhatsApp fingerprints clients at multiple layers. The fork hardens seven of them:

| Layer | Original whatsmeow | Fork |
|---|---|---|
| One-time prekeys uploaded | 30 | 520 (Chrome value) |
| Signed prekey rotation period | — | Matches Chrome |
| DeviceProps platform | — | Chrome values |
| TLS ClientHello (JA3/JA4) | Go stdlib | uTLS HelloChrome_Auto + ALPN fix |
| newsletter.go log.Fatalf | os.Exit(1) on any error | Returns error |
| Log verbosity | Various | Reduced noise |

The risk of detection is cumulative: each non-Chrome signal is individually weak but
together they make automated detection reliable. Fix all layers, not just TLS.

---

## Library Safety (for Android)

### No stdout, no os.Exit, no panic in library code

**waLog.Logger is an interface** — implement it for logcat in the Android app; do not use
`waLog.Stdout` or `waLog.Noop` in production. The interface has 5 methods: `Infof`, `Warnf`,
`Errorf`, `Debugf`, `Sub`.

**newsletter.go had `log.Fatalf`** which calls `os.Exit(1)` and kills the Android process.
Fixed to `return error`. Any future additions to the fork must never use `log.Fatal`, `log.Fatalf`,
`os.Exit`, or bare `panic` in code paths reachable from normal operation.

**send.go has one legitimate panic** (`proto.Marshal` failure) — this is low-risk since it
only fires on internal serialisation corruption, not on any user input or network condition.

---

### gomobile API boundary

gomobile's `bind` cannot export `interface{}`, `func`, channels, or maps across the boundary.
The `whatsbot-go` wrapper package solves this:

- **Listener interface** with concrete method signatures — Kotlin implements this
- **Message struct** with only primitive-compatible fields (string, int64, bool)
- Event dispatch via `emit(func(Listener))` protected by `sync.RWMutex`

whatsmeow's raw `AddEventHandler(func(evt any))` is not gomobile-compatible and must stay
inside the wrapper package, never exposed.

---

## What Failed / Traps

| What was tried | What went wrong | Correct approach |
|---|---|---|
| Patch `HandshakeState.Hello.AlpnProtocols` | Overwritten by `ApplyConfig()` during handshake | Patch the `ALPNExtension` object in `uconn.Extensions` |
| Two separate `sql.Open` calls (one for whatsmeow, one for messages) | `database is locked` under history sync load | One shared `*sql.DB` via `sqlstore.NewWithDB` |
| Stop backfill when `stored == 0` | Terminates prematurely if messages already exist in DB | Stop when `returned == 0` (WhatsApp returned empty batch) |
| Apply `--chat` filter to ON_DEMAND responses | Silently discards the messages we explicitly requested | Bypass filter for ON_DEMAND sync type |
| `go build ./...` to rebuild binary | Compiles all packages but emits no binary when multiple packages match | `go build -o ./cmd/test-connect/test-connect ./cmd/test-connect/` |

---

## Schema Reference

```sql
wa_messages (
    id          TEXT PRIMARY KEY,   -- WhatsApp message ID
    chat_jid    TEXT NOT NULL,
    sender_jid  TEXT NOT NULL,
    timestamp   INTEGER NOT NULL,   -- Unix seconds
    body        TEXT,
    media_type  TEXT,               -- NULL, "image", "video", "audio", "document", "sticker", "location", "other"
    status      TEXT,               -- WebMessageInfo_Status.String()
    push_name   TEXT,               -- sender display name at time of message
    is_from_me  INTEGER DEFAULT 0,
    is_group    INTEGER DEFAULT 0,
    is_deleted  INTEGER DEFAULT 0   -- 1 if REVOKE received
)

wa_chats (
    jid                  TEXT PRIMARY KEY,
    name                 TEXT,
    description          TEXT,
    is_group             INTEGER DEFAULT 0,
    archived             INTEGER DEFAULT 0,
    pinned               INTEGER DEFAULT 0,   -- 1 if pinned
    last_msg_ts          INTEGER,             -- Unix seconds
    ephemeral_expiration INTEGER DEFAULT 0,   -- seconds, 0 = off
    created_at           INTEGER,
    created_by           TEXT
)

wa_group_members (
    chat_jid   TEXT NOT NULL,
    member_jid TEXT NOT NULL,
    rank       TEXT DEFAULT 'regular',  -- REGULAR / ADMIN / SUPERADMIN
    PRIMARY KEY (chat_jid, member_jid)
)

wa_reactions (
    message_id  TEXT NOT NULL,
    chat_jid    TEXT NOT NULL,
    reactor_jid TEXT NOT NULL,
    emoji       TEXT NOT NULL,
    timestamp   INTEGER NOT NULL,   -- Unix seconds
    PRIMARY KEY (message_id, reactor_jid)
)
```

Schema is forward-migratable: new columns added via `ALTER TABLE ADD COLUMN` at startup,
errors silently ignored (column already exists = no-op).

---

## Artifacts

| File | Location | Purpose |
|---|---|---|
| `utls.go` | `whatsmeow-fork/` | Chrome TLS fingerprint + ALPN fix |
| `cmd/test-connect/main.go` | `whatsmeow-fork/` | CLI: pair, sync, backfill |
| `client.go` | `whatsbot-go/` | gomobile-compatible wrapper |
| `storage.go` | `whatsbot-go/` | All SQLite read/write functions |
| `listener.go` | `whatsbot-go/` | Listener interface definition |
| `message.go` | `whatsbot-go/` | Message struct + extractBody |
| `storage_test.go` | `whatsbot-go/` | Storage layer unit tests (20 tests) |
| `message_test.go` | `whatsbot-go/` | extractBody unit tests |
