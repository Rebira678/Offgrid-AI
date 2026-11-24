# ADR 014: Blobs sync by have/want with resume; collection waits out a grace period; a full disk refuses and says why

**Status:** accepted, 2026-11-22 (Day 48 of the Days 31 to 60 plan). Continues ADR 013.

## Decision

1. **Have/want exchange.** One signed `POST /sync/blobs/check` carries the hashes the caller *wants* (named by ops it holds, missing on disk) and the ones it *offers* (held). The answer lists which wanted blobs the peer *has*, and which offered blobs the peer *needs*, each with the byte offset of any partial copy it already holds, plus refusals with a reason. A blob the peer already holds is never sent; a blob no one asked for is never requested. A round with no attachments makes no blob request at all.
2. **Transfer.** `GET /sync/blob/{hash}` (honours `Range: bytes=N-`) and `PUT /sync/blob/{hash}?size=N` with `X-Offgrid-Offset`. Both stream (32 KiB at a time; a 48 MiB blob crosses the wire with under 24 MiB heap growth in the test). `Client.Once` runs the blob pass after the op pass; a blob failure is reported (`Report.BlobErr`) and never fails the op sync.
3. **Resume.** Bytes received land in `tmp/part-<hash>`; a cut connection leaves them. The next attempt continues from their length (a Range request for pulls; the offset the peer reports for pushes, adopted on a 409). Nothing becomes visible under the final name until the whole file hashes right, so a wrong continuation is caught, the partial is discarded and the next try starts from zero. Partials older than 7 days are swept. A blob already being received in this process is skipped, not doubled.
4. **A few at a time.** Two transfers at once by default (`BlobOptions.Parallel`), three attempts per blob per pass with a pause, ten minutes per blob. Standard library only (a 30-line limiter): the module proxy is not reachable from the build sandbox, so `errgroup` was not an option.
5. **Authorization.** A peer may upload only a blob some op in the view names, with exactly the size that op declares; everything else is 403. So an enrolled device cannot use another as free storage. Blob requests carry no body to sign: in shared-key mode the signature covers method, path, node and time; in mutual TLS every check ops get applies (certificate, enrolment, freshness, replay). Integrity is the SHA-256, checked on receipt. Blob replies are not signed. The server extends its read and write deadline for blob requests (the 30 s and 60 s that suit JSON batches would cut a 25 MiB photo on a weak link).
6. **Free-space guard.** Before a blob is accepted (local add, pull, or a peer's upload) the disk must have `need + reserve` free, where `need` counts only the bytes still missing in a resume and the reserve defaults to 64 MiB (`Options.BlobReserveBytes`) so the database, its WAL and the OS keep room. Measured with statfs on unix; on other platforms it cannot be measured and the filesystem's own "no space" error is mapped to the same refusal. The error says what was needed, what is free and what is reserved. A full disk stops a pull pass (no downloads are started), and a peer that says it is full is not sent the blob.
7. **Collection** (`offgrid store -db F gc`): removes blobs that no op in the log names and that are older than the grace period (default 7 days, never under 10 minutes). Age is when the blob was stored or last stored again (`Put` refreshes it, so re-adding an old orphan protects it). "Named" is judged against the whole log, not the view. `-dry-run` says what would go.
8. **Dashboard**: the task table lists attachments with thumbnails for pictures; a file whose op has synced but whose bytes have not is shown as "not on this device yet". Files are served by task and hash only, with the declared type, `nosniff`, `Content-Security-Policy: default-src 'none'; sandbox`, inline only for pictures, with Range support. The dashboard cannot add, change or delete attachments.

## Why the grace period

The bytes and the op reach a replica separately and in either order, and a local add stores the blob a moment before it writes the op. A collector that deleted every unnamed blob would sometimes delete a photo whose op was about to appear. Grace turns that race into "older than a week and still unnamed", which is a real orphan: a crash between the two writes, or a file left by a failed transfer.

## Costs and limits

- **Nothing is removed that an op names.** Deleting a task does not free its photos, and there is still no remove-attachment op. Collection frees only orphans; a purge policy for deleted tasks is a product decision for the pilot.
- **The offer list is the whole set of held blobs**, 500 hashes per request, every round. Fine at pilot scale (thousands of photos is a few dozen small requests); it should be skipped when nothing changed if it ever matters. No measurement of this at scale yet.
- **A request may declare a size for a hash that differs from a lying op's**; such a blob never matches and is never fetched. We accept it as a cost of a peer with a forged op.
- **No bandwidth limit** and no scheduling by network type: a pass moves everything it can.
- **The hub keeps blobs too** (each group's database has a `.blobs` folder beside it): hub disk use grows with the photos of the groups it hosts, and nothing but this collection frees it. Blob uploads to the hub have their own body limit (`Options.MaxBlobBody`, default 26 MiB; the 16 MiB JSON limit would have refused a large photo). Each blob request counts against the group's rate limit, so a big first sync can be throttled (429): the blob is retried next round.
- Not encrypted at rest; EXIF/GPS is kept (see ADR 013).

## Rejected

- **Pushing blobs unasked**: wastes the one expensive resource (mobile data) and needs per-peer bookkeeping the exchange gets for free.
- **Signing blob bodies** (HMAC over 25 MiB needs the whole body in memory, or a second pass): the hash already binds the bytes.
- **Collecting by view** (what the view shows): a rebuild or an op that folds differently later could make a live photo look unreferenced.
- **Percentage-only free-space rule**: on a small SD card 5% can be less than a photo; an absolute reserve is easier to explain.
