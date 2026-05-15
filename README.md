# whatsmeow — fingerprinting-hardened fork

> **The fork's changes are on the [`whatsbot-fingerprint-hardening`](../../tree/whatsbot-fingerprint-hardening) branch, not `main`.**
> Clone with: `git clone -b whatsbot-fingerprint-hardening https://github.com/caseyng/whatsmeow.git`

A fork of [tulir/whatsmeow](https://github.com/tulir/whatsmeow) that reduces the fingerprinting surface of the library when used as a WhatsApp companion device. Full credit and gratitude to [Tulir Asokan](https://github.com/tulir) for the original library.

---

## Why this fork exists

Stock whatsmeow is detectable by WhatsApp at the protocol level through several static fingerprints — values baked into the library that match no official client. A server-side classifier seeing these values can identify the connection as a non-official client with high confidence.

This fork addresses the fingerprinting vectors identified in **["Prekey Pogo: Investigating Security and Privacy Issues in WhatsApp's Handshake Mechanism"](https://arxiv.org/abs/2504.07323)** (USENIX WOOT 2025, arXiv 2504.07323). Table 4 of that paper maps prekey and device identity fields to specific clients; the original whatsmeow values produced a reliable library fingerprint across all of them.

The changes in this fork bring the library's observable behaviour in line with Chrome's companion-mode fingerprint (the same profile used by WhatsApp Web in a browser).

---

## Changes from upstream

### 1. Prekey batch size: 50 → 812 (`prekeys.go`)

`WantedPreKeyCount` controls how many prekeys are uploaded in a refill batch. The original value (50) matched no official client and appeared in Table 4 of the Prekey Pogo paper as a whatsmeow-only fingerprint. 812 matches the refill batch size used by official Android and iOS clients.

### 2. Prekey refill threshold: 5 → 10 (`prekeys.go`)

`MinPreKeyCount` is the server-side prekey count that triggers a refill upload. The original value (5) was whatsmeow-only. 10 matches the refill trigger threshold used by all official clients (Android, iOS, Web, macOS, Windows Desktop) per Table 4.

### 3. Device identity: `whatsmeow` / `UNKNOWN` / `0.1.0` → `Chrome` / `CHROME` / `124.0.0` (`store/clientpayload.go`)

`DeviceProps.Os`, `PlatformType`, and the app version are sent during device pairing. The original values explicitly identified the client as whatsmeow. This fork sets them to Chrome/124.0.0, matching the companion-mode fingerprint of a Chrome browser extension.

### 4. Signed prekey rotation every 30 days (`prekeys.go`, `store/`, DB migration 15)

Official WhatsApp clients rotate their signed prekey approximately monthly for forward secrecy. whatsmeow never rotated it — an account years old with an unchanged signed prekey since registration is an anomalous fingerprint.

This fork adds `maybeRotateSignedPreKey()`, called on every non-initial prekey refill upload. It generates a new signed prekey (incrementing the key ID), persists it, and includes it in the next upload. A new `signed_pre_key_timestamp` column (migration 15) tracks rotation age.

### 5. Chrome TLS fingerprint on WebSocket connections (`utls.go`)

Go's standard TLS library produces a distinctive JA3/JA4 fingerprint that differs from any browser. This fork adds `NewChromeHTTPClient()`, which uses [uTLS](https://github.com/refraction-networking/utls) with `HelloChrome_Auto` to impersonate Chrome's TLS ClientHello on the WebSocket connection.

```go
chrome := whatsmeow.NewChromeHTTPClient()
client.SetWebsocketHTTPClient(chrome)
client.SetPreLoginHTTPClient(chrome)
```

The ALPN extension is patched to `http/1.1` only after building the Chrome hello, because Go's `http.Transport` with a custom `DialTLSContext` cannot negotiate HTTP/2, and offering h2 causes the server to respond with binary HTTP/2 frames that the transport cannot parse.

### 6. Library safety: `log.Fatalf` → error return (`newsletter.go`)

The upstream newsletter (WhatsApp Channels) code called `log.Fatalf` on an Argo decode failure, which calls `os.Exit(1)` and kills the process. In a Go library embedded in an Android app via gomobile, this would terminate the app with no recovery path. Changed to `return fmt.Errorf(...)` so the caller can handle it.

### 7. Reduce log noise: history sync media delete (`message.go`)

After processing a history sync blob, whatsmeow attempts to delete the server-side temporary blob. This delete returns HTTP 400 if the blob was already consumed by another linked device. The upstream code logged this at `WARN` level on every sync. Downgraded to `DEBUG` — the failure is benign and not actionable.

---

## What is not changed

All message handling, session management, group operations, media upload/download, receipts, app state sync, and every other subsystem are identical to upstream. This fork makes the minimum changes needed to address the identified fingerprinting vectors.

---

## Using this fork

Add a `replace` directive in your `go.mod`:

```
require go.mau.fi/whatsmeow v0.0.0-<upstream-version>

replace go.mau.fi/whatsmeow => github.com/caseyng/whatsmeow v0.0.0-<fork-version>
```

Or pin to the hardening branch directly:

```sh
go get github.com/caseyng/whatsmeow@whatsbot-fingerprint-hardening
```

Apply the Chrome TLS client before connecting:

```go
chrome := whatsmeow.NewChromeHTTPClient()
client.SetWebsocketHTTPClient(chrome)
client.SetPreLoginHTTPClient(chrome)
```

---

## Staying in sync with upstream

This fork periodically rebases onto upstream `main`. If you are pinning a specific commit hash, check for rebase updates periodically.

---

## License

Mozilla Public License 2.0, same as the original. See [LICENSE](LICENSE).

Original library by [Tulir Asokan](https://github.com/tulir) — [tulir/whatsmeow](https://github.com/tulir/whatsmeow).
