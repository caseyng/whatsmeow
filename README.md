# whatsmeow (fingerprinting-hardened fork)

This is a fork of [tulir/whatsmeow](https://github.com/tulir/whatsmeow) with fingerprinting hardening changes. Full credit to Tulir Asokan for the original library.

whatsmeow is a Go library for the WhatsApp multi-device API. It implements the full protocol: sending and receiving messages (text and media), group management, receipts, typing notifications, app state sync, and more. See the [upstream godoc](https://pkg.go.dev/go.mau.fi/whatsmeow) for full API documentation.

---

## Changes from upstream

These changes address fingerprinting vectors identified in ["Prekey Pogo: Investigating Security and Privacy Issues in WhatsApp's Handshake Mechanism"](https://arxiv.org/abs/2504.07323) (USENIX WOOT 2025, arxiv 2504.07323). Table 4 of that paper maps prekey behavior to specific clients; the original whatsmeow values produced a reliable library fingerprint.

### 1. `WantedPreKeyCount`: 50 → 812 (`prekeys.go`)

The number of prekeys uploaded in a refill batch. The original value (50) matched no official client and appeared in Table 4 as a whatsmeow-only fingerprint. 812 matches the refill batch size used by official Android and iOS clients.

### 2. `MinPreKeyCount`: 5 → 10 (`prekeys.go`)

The server-side prekey count that triggers a refill upload. The original value (5) was whatsmeow-only. 10 matches the refill trigger threshold used by all official clients (Android, iOS, Web, macOS, Windows Desktop) per Table 4 of the paper.

### 3. DeviceProps identity: `"whatsmeow"` / `UNKNOWN` / `0.1.0` → `"Chrome"` / `CHROME` / `124.0.0` (`store/clientpayload.go`)

`DeviceProps.Os`, `PlatformType`, and the app version are sent during device pairing. The original values explicitly identified the client as whatsmeow. This fork sets them to Chrome/124.0.0, matching the companion-mode fingerprint of a Chrome browser extension.

### 4. Signed prekey rotation every 30 days (`prekeys.go`, `store/`, DB migration 15)

Official WhatsApp clients rotate their signed prekey approximately monthly for forward secrecy. whatsmeow never rotated it — an account years old with an unchanged signed prekey since registration is an anomalous fingerprint.

This fork adds `maybeRotateSignedPreKey()`, called on every non-initial prekey refill upload. It generates a new signed prekey (incrementing the key ID), persists it, and includes it in the next upload. A new `signed_pre_key_timestamp` column (migration 15) tracks rotation age. On the first run after upgrading an existing database, the timestamp is recorded without rotating — the key was already uploaded at registration.

---

## All other functionality

All other behavior is identical to upstream whatsmeow. This fork makes no changes to message handling, session management, group operations, media, or any other subsystem.

---

## Using this fork

Replace the upstream module path in your `go.mod` with a `replace` directive pointing at this fork:

```
require go.mau.fi/whatsmeow v0.0.0-<upstream-version>

replace go.mau.fi/whatsmeow => github.com/caseyng/whatsmeow v0.0.0-<fork-version>
```

Or pin to the hardening branch directly:

```sh
go get github.com/caseyng/whatsmeow@whatsbot-fingerprint-hardening
```

Then add a `replace` directive in `go.mod`:

```
replace go.mau.fi/whatsmeow => github.com/caseyng/whatsmeow v0.0.0-<resolved-hash>
```

---

## Staying in sync with upstream

This fork periodically rebases onto upstream `main`. Breaking changes from upstream (protobuf schema updates, API changes) will be carried forward. If you are pinning a specific commit hash, check for rebase updates periodically.

---

## License

Mozilla Public License 2.0, same as the original. Original library by [Tulir Asokan](https://github.com/tulir). See [LICENSE](LICENSE).
