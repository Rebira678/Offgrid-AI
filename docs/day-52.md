# Day 52: the phone client

**Done when:** a phone installs the app, goes offline, edits, reconnects, and the edit appears on the node marked late; an unpaired phone gets nothing.

**What is done and what is not.** The server, the app and the pairing are built and tested, and the whole story ran in desktop Chromium (pair, install the service worker, go offline, reload from the worker, edit, reconnect, edits applied and marked late). **No phone has run it.** The Day 33 phone test is still unrun (`docs/phone-spike-report.md` is an empty template), so this builds the private-CA PWA that Day 33 planned and says so. iOS: **untested**.

## Try it on your laptop (no phone, no certificate)

```bash
offgrid store -db site.db found -label site            # once; this node must be an admin
offgrid phone pair -db site.db -label "Test phone"     # prints a one-time code, valid 15 minutes
offgrid phone serve -db site.db -addr 127.0.0.1:8443   # plain HTTP, this machine only
# open http://localhost:8443 in Chrome, type the code, tap Pair
```

For a real phone: `offgrid ca issue ...` (Day 33), then
`offgrid phone serve -db site.db -cert node.crt -key node.key -addr :8443`, install `ca.crt` on the phone, open `https://<node address>:8443`, pair, and "Add to Home screen".

| Command | What it does |
|---|---|
| `phone pair -db F [-label L] [-ttl 15m]` | A one-time member invite, as a code. |
| `phone serve -db F [-cert -key] [-addr] [-host NAME]... [-late-after 2m] [-session-days 30]` | Serves the app. Without `-cert/-key` it refuses any address but this machine. |
| `phone reset-sessions -db F` | Signs every phone out (they sign in again with their keys). |
| `store -db F revoke -device D` | A lost phone: cut off within about 3 seconds. |

`main.go` needs one line (you merge it): `case "phone": return cmdPhone(args[1:], stdout, stderr)`, plus a usage line.

## What the app does

Task list (open or all), add a task, set status and priority, add notes, add a photo, see and accept or dismiss the AI's suggestions. It shows the last list it read when the node is unreachable, and every change shows "waiting to sync" until the node has taken it. Photos are re-drawn on the phone before upload, so they are small and carry no GPS.

## What happens to a change made offline

1. It goes in a queue on the phone with an ID and its age.
2. When the node is reachable the queue is sent. The node applies each command as a **new op stamped on arrival**.
3. If it waited 2 minutes or more, the task gets `from phone "Ana": late, made 2h05m before it reached this node`.
4. **Conflicts**: the last command to arrive wins. If the field had changed since the phone looked, the node says so on the task and the phone is told. A suggestion that went stale is refused, not forced. A task that was removed: refused.
5. Sending the same command twice does nothing the second time.

## Why an unpaired phone gets nothing

Everything under `/api/` except pairing and sign-in needs a session cookie (`__Host-offgrid`, HttpOnly, Secure, SameSite=Strict) that the node signed for an enrolled phone, and every request re-checks the phone is still enrolled. The app's own files are public and contain no task data. Wrong codes are slowed (5 a minute per address, 20 for the node). Details and the rules inherited from Day 26: ADR 018.

## What I verified

- `internal/phone`: 56 tests (real store, real signatures): no data without a session (forged, expired, other-key, garbage cookies on every route); pairing and each refusal; admin invite refused and undone; revoked phone cut off, also at sign-in during the cache window; sign-in replay, lateness and wrong-key refusals; the header, origin, fetch-metadata and Host rules; policy headers; exact late threshold; idempotence, also after a restart; create-then-edit by reference; conflict flagging; suggestions (accept, dismiss, stale, twice); every bad-command case and the limits; photos (type, size, empty, not-an-image, duplicate by hash, late); secret file permissions and rotation; TLS 1.2 minimum.
- `cmd/offgrid`: pair, serve over real TLS with a Day 33 certificate, pair as a phone would, sync, sign in again; refusals.
- **Mutation tests**: 50 mutations on the server, sessions and serve code. Every one is killed except three that are equivalent (a clamp at exactly 90 days gives the same result either way; a negative age is never late either way; the exclusive-create flag only matters in a race that `phone serve` cannot start).
- **Real browser** (`scripts/phone-e2e.js`, Chromium, localhost): 7 checks pass. It found two bugs the unit tests could not: inline `style` attributes were blocked by the policy, and a redraw wiped a half-typed note.
- Also fixed (Day 38): `store invite` printed its code grouped twice (`TK2L--FME-P-4R-...`). It still worked when typed; it now prints `TK2L-FMEP-4R5S-...`.

## Honest limits

- No phone has run this. Desktop Chromium on localhost is a secure context; a phone on `https://192.168.x.x` with your CA installed is the real test, and iOS may behave differently (storage clearing, install flow).
- The cookie is a bearer token (ADR 018).
- A promptly applied (not late) note whose reply is lost, plus a node restart before the retry, is added twice.
- Photos already on the node are not available offline.
- If the browser will not give the app storage (a private tab), the app says so and stops; there is no separate online-only build.
- A phone is not a replica: ordering is by arrival, and only one node serves it.

## Your part

1. Merge the `case "phone"` line into `cmd/offgrid/main.go`.
2. **Run Day 33 on a real Android phone** and fill `docs/phone-spike-report.md`; then pair that phone and run the story above (install, airplane mode, edit, reconnect). Write what failed.
3. Try iOS if you can borrow one, or write "iOS: untested".
4. Optional: `npm i playwright && CODE=... PIC=... node scripts/phone-e2e.js` after `phone pair` and `phone serve`.
5. Revoke a test phone (`store revoke`) and confirm it is cut off.
