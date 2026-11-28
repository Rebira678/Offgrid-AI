# API versioning policy

Offgrid's HTTP API has two prefixes and one rule for each.

| Prefix | Meaning | What we promise |
|---|---|---|
| `/v1/` | **Stable.** | It only grows. A script written against it keeps working. |
| `/v0/` | **Not yet proven.** | It may change or go away between releases. Do not build on it without pinning a version. |

Every operation in `openapi.json` carries `x-stability`, and the contract test fails if an operation under `/v1/` says `v0` or the other way round.

## What "stable" means under `/v1`

Allowed without a new version:

- adding a field to a response object;
- adding an optional field to a request;
- adding a new endpoint, query parameter (optional) or error `code`;
- making an error message clearer (clients match on `code` and the HTTP status, never on `message`).

Not allowed without `/v2`:

- removing or renaming a field, endpoint, parameter or error code;
- changing a field's type or meaning, or a status code a call returns;
- making an optional field required, or tightening a limit so that a request that worked now fails.

**Clients must ignore response fields they do not know.** The server, in turn, **rejects request fields it does not know** (HTTP 400 `bad_request`), so that a typo such as `priorty` is an error and not a silent no-op. This is why a request field can be added without a version bump but a client written for a newer server will fail against an older one.

## Cursors and pagination

A list is paged with `limit` (default 100, at most 500) and an opaque `cursor`. Treat the cursor as a string to hand back; its contents are not part of the contract.

## Deprecation

A `/v1` feature is removed only by shipping `/v2` and keeping `/v1` running for at least one release in which `/v1` answers carry a `Deprecation` header and the changelog says so. Nothing is deprecated today.

## Webhooks, CSV import

Both are `/v0`. The webhook event types, payload fields and signature scheme are written down in `docs/api/README.md` and tested, but they have no real-world user yet. CSV **export** is stable (the column order and names only grow at the end); CSV **import** is `/v0`.

## Moving something from v0 to v1

When it has been used for real for a while and nothing about it changed, it gets a `/v1` route and the `/v0` route stays as an alias for one release. The date and the evidence go in an ADR.
