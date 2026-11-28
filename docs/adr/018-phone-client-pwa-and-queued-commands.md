# ADR 018: The phone client is a PWA that queues commands, not a replica

**Status:** accepted, 2026-11-28 (Day 52). Built on the Day 33 plan (private-CA web app). **The Day 33 phone test has not been run**, so every claim below about what a phone does is unproven until `docs/phone-spike-report.md` has a date, a model and an OS in it.

## Context

A person on site wants the task list, a way to add and update tasks, photos, and the AI's suggestions on the phone they already carry, including when the node is out of reach. A native app is out of scope until after the pilot (Day 33). Putting a full replica in the browser (the op log, hash chain and signing in JavaScript) would be a second implementation of the hardest part of the system, and a phone that keeps a signing key and an op log is a device that must be revoked, audited and kept in step.

## Decisions

1. **A PWA served by the site node, over HTTPS** from the Day 33 certificate authority. It is a separate server (`offgrid phone serve`) from the localhost dashboard (`offgrid web`), which stays loopback-only and login-less. The app's own files are embedded (`go:embed`) and public; everything else needs a session.
2. **Writes are commands, not ops.** The phone keeps the last list it read and a queue of what the person did since ("set T3 done", "add this note", "accept suggestion S"). It never builds an op. On reconnecting it sends the queue; the node applies each command as a new, signed op **stamped when it arrives** and answers per command: `applied`, `duplicate` or `rejected`.
3. **"Late" is written down.** A command that waited at least 2 minutes on the phone (by the phone's own clock, so a wrong phone clock does not matter; `-late-after`) gets a note on its task: `from phone "Ana": late, made 2h05m before it reached this node [c:ID]`. A note carries it in the same op as its text. A photo is marked the same way.
4. **Conflicts: the last command to arrive wins, and says so.** There is no merge. A status or priority command carries the value the phone was showing (`was`); if the task has a different value when it arrives, the command still applies and the node writes `it replaced a newer change (priority was high, the phone had seen medium, now low)`, and the phone is told. Two exceptions are refused rather than forced: an AI suggestion whose task changed since (`stale`, the Day 30 rule) and one already resolved. A command about a task that no longer exists is `rejected`.
5. **Pairing uses the invites that already exist.** `offgrid phone pair` makes a one-time, 15-minute member invite. The phone makes an ECDSA P-256 key in the browser (WebCrypto, **non-extractable**, kept in IndexedDB), signs the enrollment message with it, and redeems the code. The node enrolls it with a signed `member.add` op (so the phone is in the membership list, can be revoked, and shows up in `store members`) and starts a session. A phone is only ever a member: an admin code is refused and the device it enrolled is revoked at once.
6. **A session is a signed cookie, bound to the phone's device ID, re-checked on every request.** `__Host-offgrid`, HttpOnly, Secure, SameSite=Strict, 30 days (at most 90). It is an HMAC over {device, issued, expiry} with a key in `phone-session.key` (mode 0600), so a node restart logs nobody out. Every request also checks the device is still an enrolled, un-revoked member (reused for at most 3 seconds), so revoking a lost phone cuts it off within seconds whatever its cookie says. A phone whose session ended signs in again by signing a one-time challenge with its key, with no new code. `offgrid phone reset-sessions` rotates the HMAC key.
7. **The rules from Day 26 apply, plus the network ones.** Strict CSP (no inline script or style; `manifest-src` and `worker-src` added), `X-Offgrid-Action: 1` on every POST, same-origin `Origin` and `Sec-Fetch-Site`, `Host` must be a name this node serves (private addresses and `.local/.lan/.offgrid` by default; `-host` narrows it), HSTS, a guess limiter on pairing and sign-in (5 failures a minute per address, 20 for the node), request bodies capped, photos pass through the same type and content checks as Day 47 and are served with the same sandboxing.
8. **Photos are re-drawn on the phone before upload** (largest side 2048, JPEG 0.85). That makes them small and removes the EXIF block, including GPS. It also resolves the open EXIF question *for phone photos only*: files attached on a computer keep theirs.

## Rejected

- **A replica in the browser.** See Context. It would give true merging and cost a second implementation of the op log.
- **Plain HTTP on the LAN.** No service worker, WebCrypto or IndexedDB without a secure context.
- **Reusing the dashboard server.** It has no login and trusts loopback; adding one would put a network-facing surface in the process that must stay local.
- **A server-side session table.** It would need a schema change and would forget sessions on migration; the HMAC token plus the membership check gives instant revocation without one.
- **Making the phone sign requests with its key (a proof-of-possession token).** Stronger against a stolen cookie, but every request needs a signature round trip, and the cookie is already `HttpOnly`/`Secure`. Revisit if the pilot shows cookie theft is real.

## What it costs

- **Weaker ordering than a replica.** An edit made at 09:00 and applied at 11:00 is stamped 11:00; if two phones edit the same field the later arrival wins. Acceptable inside one site's network; not a general answer.
- **The session is a bearer token.** Anyone who copies the cookie out of the phone can use it until it expires or the device is revoked.
- **A revoked phone has up to 3 seconds** (the membership cache) after the revoke is recorded on THIS node. Revoking on another node reaches this one when they next sync.
- **Photos are not cached for offline viewing.** The list, notes and statuses are; pictures already on the node show when connected.
- **Idempotence is complete only for late commands.** A command that was applied promptly (not late) is remembered in memory (the last 4096). If its reply is lost AND the node restarts before the phone retries, a repeated *note* is added twice. Late commands carry a marker in the log and survive restarts.
- **Pairing codes are bearer secrets for 15 minutes.** Give them in person.
- **iOS is untested.** Safari clears site data for sites not used for a while unless the app is added to the home screen; the app does not detect it.

## What would change it

The Day 33 result: if service workers, IndexedDB or the CA install fail on the phones you test, the roadmap's fallback is an online-only page; the app already stops with a clear message when the browser will not store data. The pilot: if people need to merge edits made offline, that is the case for a replica.

## Evidence so far

- 61 Go tests in `internal/phone` and `cmd/offgrid` (a real TLS server, a real pairing, real sign-in; mutation-tested).
- A real Chromium run (`scripts/phone-e2e.js`): pair, install the service worker, go offline, reload from the worker, make a task, set another done, add a note and a photo, reconnect; every change arrives and is marked late. Desktop Chromium on localhost, **not a phone**.
- It found two bugs a unit test could not: inline `style` attributes blocked by the policy, and a redraw wiping a half-typed note.

## Where the implementation stands

`internal/phone` (server, sessions, replay), `internal/phone/static` (the app), `cmd/offgrid/phonecmd.go`. `web.Work` and `web.ServeAttachment` were factored out of the dashboard so both read tasks and serve files the same way.
