# ADR 007: Phone client PWA, pairing tokens, and offline queued writes

**Status:** accepted, 2026-11-26 (Day 52).

## Context

Field technicians work on mobile devices with intermittent or zero network connectivity. The Day 33 spike demonstrated on real Android hardware that Chrome can reliably serve an installable Progressive Web App (PWA) offline using a Service Worker and IndexedDB, verified across 7 offline page reloads. However, phones cannot act as full continuous replication nodes: mobile OS battery governors freeze background processes, browsers evict storage under memory pressure, and mobile devices cannot listen on persistent inbound ports for peer-to-peer sync.

## Decisions

1. **A PWA served directly by the site node.** Everything is embedded into the single binary via `go:embed` (HTML, JS, CSS, Web Manifest, Service Worker). No CDN, no third-party assets, and zero external network traffic required.
2. **Pairing codes and session tokens.** An enrolled node operator runs `offgrid phone pair` to issue a short-lived (15-minute TTL), single-use 6-digit code (`XXX-XXX`). The phone enters this code to redeem a 256-bit cryptographically secure session token stored in an `HttpOnly`, `SameSite=Strict`, `Secure` cookie (and Bearer header). An unpaired phone gets nothing (`401 Unauthorized` on all data endpoints).
3. **Localhost dashboard stays separate.** The operator dashboard (`offgrid web`) continues to listen strictly on loopback (`127.0.0.1`, `::1`) with no login and no encryption. The field client (`offgrid phone serve`) is distinct, designed for LAN access with mutual TLS or private CA HTTPS and authenticated sessions.
4. **Strict Content Security Policy (CSP) and CSRF header.** Keeps the strict CSP from Day 26 (`default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data: blob:; manifest-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`). All mutating requests require `X-Offgrid-Client: phone` (or `X-Offgrid-Action: 1`) plus same-origin validation.
5. **Offline reads and queued writes.** The phone caches static assets in CacheStorage and reads tasks from IndexedDB when offline. When offline, edits (task creation, status changes, notes, deletions, AI suggestion resolves) are appended to an IndexedDB queue and optimistically reflected in the local UI.
6. **Replay stamped on arrival, marked "late".** When the phone reconnects, it POSTs its queue to `/api/phone/sync/replay`. Queued writes are commands replayed later, not a merged replica. The node stamps each operation with its current node clock on arrival and annotates it as late (e.g., `[late sync from phone <label> offline at <time>]`).
7. **Conflict handling.** If a task was deleted on the node while the phone was offline, the node marks the replayed edit as `conflict_task_deleted` without aborting the rest of the batch. If an AI suggestion became stale or was already resolved, the node returns `conflict_stale`.

## Rejected

- **Running a full CRDT replica inside the phone browser.** Mobile browsers aggressively throttle background tabs, clear IndexedDB under pressure, and cannot maintain listening sockets. Commands replayed to the node are vastly simpler and more resilient.
- **Passwords or user logins.** Field deployments have no centralized identity provider. A time-bounded, single-use pairing code entered once binds the session token to the physical device.
- **Honoring phone timestamps as absolute authority.** Clocks on field phones are frequently skewed. Arrival time on the node determines log ordering.

## What it costs

Weaker ordering than a true replica: an edit made offline on a phone is ordered after events that occurred while it was away, even if the user physically acted earlier. This is acceptable inside a site network where field notes and inspections describe ongoing maintenance.

## What would change it

If pilot users require phone-to-phone direct sync without passing through a site node, a native client holding a true SQLite replica would be required.
