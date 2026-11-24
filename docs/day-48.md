# Day 48: attachments, part 2

Photos now travel between devices, survive a bad connection, and cannot fill the disk.

## What was built

- **Blob sync** (`internal/syncer/blobsync.go`): have/want check, `GET` with Range, `PUT` with offset, a few transfers at a time, resume on both directions, `Report.Blobs` and `Report.BlobErr`. `Client.Once` does it after the ops; `NoBlobs` turns it off.
- **Resumable receive** (`blobstore.Transfer`): partial files, checked against the hash only when complete; partials swept after 7 days.
- **Free-space guard** (statfs): `need + reserve`, numbers in the error, applied to local adds, pulls and uploads.
- **Collection and stats** (`Store.CollectBlobs`, `BlobStats`): unnamed blobs older than the grace period; `Put` refreshes age so a re-add is safe.
- **Dashboard**: attachments listed and shown, served with a locked-down response.
- **Commands**: `offgrid store -db F attach -task T3 -file photo.jpg`, `attachments -task T3`, `blobs`, `gc [-grace 168h] [-dry-run]`; `offgrid sync` prints what files moved and why any did not.
- **Tests**: blobstore 13 more, store 9, syncer 17, web 5, command line 3. ADR 014.

## What I measured

- A 48 MiB blob crosses HTTP with under 24 MiB of heap growth; a 64 MiB resumed transfer under 8 MiB.
- A pull cut at 350,000 of 900,000 bytes resumed with a Range request: the server wrote 900,000 bytes in total, not 1,250,000.
- A push cut at 300,000 of 800,000 bytes: the retry sent only the missing part.
- 8 blobs with a cap of 3: never more than 3 at once.
- Mutation checks, all caught: no Range on resume; upload without the "an op names it" check; resending what the peer has; no cap on parallelism; no space check in the have/want answer.

## Honest limits

- Everything above ran on loopback and in this container's filesystem. **No real weak link, no real SD card, no real full disk.**
- The whole held-blob list is offered every round (500 per request). Fine at pilot scale, unmeasured beyond it.
- Deleting a task does not free its photos; only orphans are collected. No remove-attachment op.
- There is no way to add an attachment from the dashboard (read-only on purpose); the command line adds them.
- No bandwidth limit, no "only on Wi-Fi" rule.
- Older replicas refuse the Day 47 op: upgrade all devices first.
- Photos keep GPS/EXIF.

## Your part

1. **Two real devices**: on the laptop `offgrid store -db F attach -task T1 -file some.jpg`, sync with the Pi, then on the Pi `offgrid store -db F attachments -task T1` (should say "bytes here") and open the dashboard. Then pull the Wi-Fi mid-transfer with a 20 MB file and sync again: the report should say "resumed".
2. **Full disk**: on the Pi, make the card nearly full (or run with a small partition) and sync: it should refuse with the numbers, not fill the card. Report what the message says.
3. Run `offgrid store -db F gc -dry-run` on a database that has been used for a week.
4. Decide the EXIF/GPS question and whether deleted tasks should free their photos.
5. Open items from before (Day 46 power pull and modernc numbers, Checkpoint 1, goleak, main.go merge, benchmarks).
