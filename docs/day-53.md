# Day 53: stable API v1, OpenAPI, webhooks, CSV

**Done when:** a script can list, create and update tasks over `/v1`; the spec matches the server; a webhook receiver can verify a signed event; a CSV export imports back.

**What is done and what is not.** Built and tested end to end in this repository. **No outside caller has used it**, no webhook has been delivered to a real third-party service, and no CSV has come from real Excel or Sheets. That is why only `/v1` (tasks, notes, attachments, suggestions, CSV export) is called stable, and webhooks and CSV import are `/v0`.

## Try it

```bash
offgrid api serve -db site.db &                       # 127.0.0.1:8090, this machine only
curl -s localhost:8090/v1/tasks | head
offgrid api spec > openapi.json
offgrid api export -db site.db > tasks.csv
offgrid api import -db site.db -dry-run tasks.csv

offgrid api webhook add -db site.db -url https://example.com/hook -event task.created
offgrid api webhook test -db site.db -id wh_...       # sends a signed ping
offgrid api webhook run  -db site.db
```

Full reference: `docs/api/README.md`. Policy: `docs/api/versioning.md`. Decision: `docs/adr/019-stable-api-v1-webhooks-csv.md`.

## Wiring (yours)

`cmd/offgrid/main.go` needs `case "api": return cmdAPI(args[1:], stdout, stderr)` and a usage line. (Your `case "phone"` from Day 52 too, if not yet merged.)

## Tests, and what they cover

- **Contract:** every route's real responses validated against `openapi.json`; every declared status seen; route table equals spec; stability equals prefix.
- **API:** pagination with ties and UTC, filters, create idempotence, patch, attachments round trip, suggestions, unknown routes and 405 with `Allow`, import gating, a failing store giving 500 with no leaked text, the local guard.
- **CSV:** escaping is reversible (random strings), a round trip keeps every task, finished and open tasks with the same title both survive, bad-row reports, dry run, size and row limits, cancellation.
- **Webhooks:** signing and tolerance edges, URL validation, event mapping with and without text, hooks file mode 0600 and atomic write, retry rules per status code, redirects not followed, dead receiver not blocking others, cancellation not losing an event, cursor and `From` behavior, restart does not resend.
- **CLI:** every `api` command, loopback refusal, write header and Host checks on the live server, round trip through files, oversize file refused whole.
- **Phone mount:** 401 without a session, same CSRF rules for PATCH, revoked phone cut off, no import, photo limit follows the phone's config.
- **Mutations:** a list of deliberate bugs was applied to the API, CSV, guard, webhook, CLI and phone-mount code; each is caught by a test, or listed below as equivalent.

## Honest limits

- **The phone app does not use `/v1`.** It keeps its own `/api/state` and `/api/sync`. `/v1` is for scripts and a future native app.
- **No tokens.** The local server trusts anything on 127.0.0.1; remote callers need a paired phone session.
- **Webhooks are best effort.** At least once while the runner is up; about 15–20 seconds of retrying a dead receiver, then the event is dropped and logged. The saved position lives in the hooks file; if you edit that file by hand and lower `cursor`, events after it are sent again (each hook still skips anything from before it was added).
- **Import keeps content, not IDs or timestamps**, and is `/v0`.
- **The spec is hand-kept.** The validator checks structure, types, statuses and headers, not descriptions or examples.
- **Equivalent mutants left alone:** raising the dispatcher cursor to the oldest hook's start (an optimisation; each hook is checked on its own); `CreatedAt.UTC()` (the store already returns UTC; kept as defence); an empty-patch check the store repeats.

## Your part

1. Merge the `api` case (and `phone`) into `cmd/offgrid/main.go`.
2. Run `offgrid api serve` against a real database and try the curl examples.
3. Point a real receiver at `webhook add` (a request-catcher site works) and check the signature with the Python snippet.
4. Export, open in Excel/Sheets, save as CSV, import with `-dry-run`.
5. Tell me whether the `/v1` shapes feel right before anyone depends on them.
