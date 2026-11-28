# The Offgrid HTTP API

`openapi.json` (OpenAPI 3.0.3) is the contract: `offgrid api spec` prints it and `GET /v1/openapi.json` serves it. A test checks every real response against it. The policy is in `versioning.md`.

## Who can call it

| Where | How it is protected |
|---|---|
| `offgrid api serve -db FILE` | Listens on 127.0.0.1 only and has **no login**: any program on this machine can use it. Writes need the header `X-Offgrid-Action: 1`; a wrong `Host`, `Origin` or `Sec-Fetch-Site` is refused. |
| `offgrid phone serve` (same `/v1`) | The phone's signed-in session cookie, checked against the membership on every request. A revoked phone is cut off within seconds. No bulk import. |

There are **no API tokens**. A script on another machine has to go through a paired device or run on the node.

## Examples

```bash
offgrid api serve -db site.db &            # http://127.0.0.1:8090
B=http://127.0.0.1:8090
H='X-Offgrid-Action: 1'

curl -s "$B/v1/info"
curl -s "$B/v1/tasks?status=open&limit=50"
curl -s -H "$H" -H 'Content-Type: application/json' -d '{"title":"Fix pump","site":"4","priority":"high"}' "$B/v1/tasks"
curl -s -X PATCH -H "$H" -H 'Content-Type: application/json' -d '{"status":"done"}' "$B/v1/tasks/T1"
curl -s -H "$H" -H 'Content-Type: application/json' -d '{"text":"parts ordered"}' "$B/v1/tasks/T1/notes"
curl -s -H "$H" -H 'Content-Type: image/jpeg' -H 'X-Offgrid-Name: pump.jpg' --data-binary @pump.jpg "$B/v1/tasks/T1/attachments"
curl -s "$B/v1/suggestions"
curl -s -H "$H" -X POST "$B/v1/suggestions/S1/accept"
```

Creating a task whose title and site match an unfinished task returns **200 with the existing task**, not 201 (the same dedupe as everywhere else).

Every failure is `{"code": "...", "message": "..."}` with the HTTP status that goes with it. Codes: `bad_request`, `invalid`, `not_found`, `stale`, `already_resolved`, `too_large`, `unsupported_media_type`, `method_not_allowed`, `internal`; from the guard in front: `forbidden`, `missing_header`, `bad_host`, `cross_origin`, `cross_site`; on the phone server also `unpaired` and `slow_down`. Match on `code`, never on `message`.

## CSV

```bash
offgrid api export -db site.db > tasks.csv                  # or GET /v1/export/tasks.csv   (stable)
offgrid api import -db site.db -dry-run tasks.csv           # or POST /v0/import/tasks.csv?dry_run=true   (v0)
offgrid api import -db site.db tasks.csv
```

Columns: `id,title,site,status,priority,created_at,updated_at,done_at,notes`. `notes` is one cell holding a JSON array of strings (a note can contain any character). A cell that starts with `=`, `+`, `-`, `@`, tab or carriage return gets a `'` in front on export so a spreadsheet will not run it as a formula; import removes exactly one. Import needs only `title`; it ignores unknown columns, strips an Excel byte order mark, and treats a plain-text `notes` cell as one note.

Import **keeps content, not identity**: new IDs, new timestamps (now). It never changes an existing task: a row whose unfinished task already exists is skipped and listed. Each bad row is reported by its row number and does not stop the others. Limits: 8 MiB and 10 000 rows.

## Webhooks (v0)

Set up on the machine, never through the API (a URL the node will POST to is a way to make it call anything on your network, so only the person at the keyboard may add one).

```bash
offgrid api webhook add -db site.db -url https://example.com/hook -event task.created -event task.updated
offgrid api webhook run -db site.db          # keep this running (a service, tmux)
offgrid api webhook list|test|remove ...
```

A hook sees only changes made **after** it was added. Event types: `task.created`, `task.updated`, `task.note_added`, `task.removed`, `suggestion.made`, `suggestion.resolved`, `attachment.added`, `ping`.

```json
{"id":"site:42","type":"task.updated","at":"2026-11-28T09:00:00Z","node":"site","task":"T1","data":{"status":"done"}}
```

`id` is `node:sequence` and is the same on every retry, so use it to ignore duplicates. Events carry IDs and the values that changed. Titles, notes and file names are included only if the hook was added with `-include-text`.

**Headers:** `X-Offgrid-Event`, `X-Offgrid-Delivery` (the event id), `X-Offgrid-Timestamp` (unix seconds), `X-Offgrid-Signature`: `v1=` + hex HMAC-SHA256 of `timestamp + "." + body` with the hook's secret.

Verify in Python:

```python
import hmac, hashlib, time
def ok(secret, sig, ts, body: bytes):
    if abs(time.time() - int(ts)) > 300: return False
    mac = hmac.new(secret.encode(), ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(sig, "v1=" + mac)
```

**Delivery** is at least once and best effort: answer any 2xx. A network error, 5xx, 408 or 429 is retried (three tries, 1 s then 4 s apart, 5 s timeout each); another 4xx or a redirect is final (redirects are not followed). After that the event is dropped and logged; the runner moves on rather than stall behind one dead receiver. If the runner is stopped, it resumes from its saved position and does not resend what it delivered.
