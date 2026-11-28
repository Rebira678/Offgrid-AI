# ADR 019: A stable `/v1` API with an OpenAPI contract; webhooks and CSV import stay `/v0`

**Status:** accepted, 2026-11-28 (Day 53). Nothing here has had a real user yet. `/v1` is stable because we **promise** it will be, not because anyone has depended on it.

## Context

Until now the only HTTP surfaces were the dashboard's and the phone app's own endpoints, which change whenever the screens change. Someone who wants a script, a spreadsheet export or a notification needs something that will not move under them. The danger is promising too much too early: a promise we break is worse than no promise.

## Decisions

1. **Two prefixes.** `/v1/` is stable and only grows; `/v0/` is unproven and may change. The rules are in `docs/api/versioning.md`. Every operation in the spec carries `x-stability`, and a test fails if it disagrees with the prefix.
2. **The spec is the contract and is enforced.** `internal/api/openapi.json` is embedded and served. A test calls every route and checks the real responses (status, headers, body) against it with a small schema validator, requires every declared status except 500 to have been seen, and requires the route table and the spec to list the same operations. The spec is written by hand (generated once by a script, then edited); the test is what keeps it honest.
3. **One error shape**, `{code, message}`; clients match on `code`. Store errors map to fixed statuses; anything unexpected is 500 with a generic message, never the error text.
4. **Request fields the server does not know are an error**, so a typo is not a silent no-op. Response fields may be added, and clients must ignore ones they do not know.
5. **Pagination by keyset cursor** (creation time, then ID), not by offset, so a list does not skip or repeat tasks when one is added during paging. The cursor is opaque.
6. **Authentication is not part of the API package.** It is mounted by whoever serves it: `offgrid api serve` (127.0.0.1 only, no login, `X-Offgrid-Action` on writes, Host/Origin/Sec-Fetch-Site checks) and the phone server (behind the signed-in session, with PATCH added to its method list for `/v1` and `/v0` only). Bulk import is mounted on the local server only.
7. **No API tokens.** A token that can write tasks from another machine is a second credential system next to device keys and revocation. Until there is a real need, remote callers go through a paired device.
8. **Webhooks are `/v0` and are configured only from the command line.** A URL the node will POST to would let anyone who can call the API make the node call any address on the owner's network (SSRF), so the API has no endpoint that adds one. Events carry IDs and changed values; titles, notes and file names only if the hook was added with `-include-text`, because a webhook receiver is usually someone else's server. Deliveries are signed (HMAC-SHA256 over timestamp and body, 5-minute tolerance) and **at least once, best effort**: three tries over about 5 seconds plus the time-outs, then dropped and logged. Redirects are not followed. A hook sees nothing from before it was added.
9. **CSV export is stable; import is `/v0`.** Export escapes cells that a spreadsheet would run as a formula and import undoes exactly that. Import never changes an existing task, reports every bad row, and has a dry run. It keeps content, not IDs or timestamps.

## Rejected

- **Offset pagination.** Skips and repeats under writes.
- **Generating the spec from code, or the code from the spec.** Either adds a toolchain; a contract test gives most of the benefit.
- **A webhook endpoint in the API.** See 8.
- **Exactly-once delivery.** It needs the receiver's cooperation. The event ID lets the receiver deduplicate.
- **Moving the phone app onto `/v1`.** The app works and is tested; moving it now would change what is being tested for no new capability. It may move once `/v1` has had use.

## What it costs

Two API surfaces for tasks now exist (the phone's `/api/sync`, and `/v1`). A field once in `/v1` is there for good. The hand-kept spec can drift from the code in ways the validator does not look at (descriptions, examples).

## What would change it

A real user who needs a remote script (tokens, ADR to follow); a receiver that needs a guarantee we do not give (a delivery log on disk); a `/v1` mistake that cannot be fixed additively (`/v2`).

## Where the implementation stands

Built and tested: `internal/api`, `internal/webhook`, `offgrid api …`, the phone mount. **Not done:** no one has called it from outside our tests; the phone app does not use it; webhooks have not been run against a real third-party receiver; the CSV import has not been tried with a file out of real Excel or Google Sheets.
