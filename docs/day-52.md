# Day 52: Phone client, pairing tokens, and offline queued writes

Decisions in [ADR 007](adr/007-phone-pwa-offline-queued-writes.md); PWA assets and engine in `internal/phone/static/` and `internal/phone/`.

## What you can do now

```bash
# 1. Start the phone client PWA on the site node
offgrid phone serve -db ~/offgrid/tasks.db -addr :8443 -cert node.crt -key node.key

# 2. Issue a short-lived pairing code for a field technician's phone
offgrid phone pair -db ~/offgrid/tasks.db -label "Field Tech Alice" -ttl 15m
# Output:
# Offgrid Phone Pairing
# ---------------------
# Pairing code: 492-817
# Expires:      in 15m0s (2026-11-28 05:45:00 UTC)
# Label:        Field Tech Alice

# 3. List active enrolled phones
offgrid phone list -db ~/offgrid/tasks.db

# 4. Revoke an enrolled phone by token or prefix if lost or stolen
offgrid phone revoke -db ~/offgrid/tasks.db -token 4f8a12e3
```

## How it works

1. **Embedded PWA via `go:embed`**:
   The entire web application (`index.html`, `app.js`, `app.css`, `sw.js`, `manifest.webmanifest`, `icon.svg`) is compiled directly into the `offgrid` binary. No CDN, no third-party scripts, and zero internet connectivity required.
2. **Pairing and Cryptographic Sessions**:
   An unpaired phone gets nothing (`401 Unauthorized` on all data routes). When navigating to the node, the phone enters the 6-digit pairing code (`XXX-XXX`), which burns the code and issues a 256-bit cryptographically secure session token stored in an `HttpOnly`, `SameSite=Strict`, `Secure` cookie (and Bearer header).
3. **Offline Reads and Queued Writes**:
   The Service Worker caches static assets for offline launch (verified across real device loads in Day 33 spike). Tasks are cached in IndexedDB for offline inspection. When a technician edits offline (creates a task, updates status, adds notes, deletes, or resolves AI suggestions), operations are stored in an IndexedDB queue and optimistically rendered.
4. **Replay on Reconnect Stamped "Late"**:
   When the phone reconnects to the site network, it sends its queue to `/api/phone/sync/replay`. The node applies each operation as a new op stamped with the node's current arrival clock and marks it as late (e.g. `[late sync from phone <label> offline at <time>]`).
5. **Conflict Handling**:
   If a task was deleted on the node while the phone was away, the replay reports `conflict_task_deleted` without halting the batch. If an AI suggestion was concurrently modified or resolved, it reports `conflict_stale`.
6. **Strict Security Boundaries**:
   The operator dashboard (`offgrid web`) remains strictly bound to localhost loopback. The phone server (`offgrid phone serve`) exposes the authenticated PWA with strict CSP (`default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data: blob:; manifest-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`) and custom header validation (`X-Offgrid-Client: phone`) against cross-site requests.

## Verified

- `go test -v -race ./internal/phone ./cmd/offgrid`: 100% pass across all pairing, auth, replay, conflict, attachment, and CLI tests with zero race conditions.
- `make wasm-test`: standard library purity preserved, WebAssembly tests pass.
- `test -z "$(gofmt -l .)" && go vet ./...`: 0 issues.
- Comprehensive edge testing: malformed pairing codes, boundary expirations, concurrent duplicate redemptions, oversized payloads, path traversal rejection, and read-only mode protections.
