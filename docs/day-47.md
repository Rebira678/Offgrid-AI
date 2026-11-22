# Day 47: attachments, part 1

A task can now carry a photo or PDF. This is the storage half; moving blobs between devices is Day 48.

## What was built

- **`internal/blobstore`**: content-addressed file store. `Put`, `PutExpected` (bytes from a peer, checked against the hash), `Open`, `Has`, `Verify`. Size cap, atomic writes, streaming, private permissions, crash leftovers swept.
- **`attachment.added` op** (in `internal/store/sqlstore`): `{task, hash, size, type, name}`. Applied into a new `attachments` table (projection version 5). Add-wins against a concurrent delete, inert on void tasks, refused when malformed.
- **API**: `Store.AddAttachment(ctx, taskID, reader, declaredSize, mediaType, name)`, `Attachments`, `OpenAttachment`, `Blobs`. Options `BlobDir`, `MaxBlobBytes`.
- **Tests**: 16 in `blobstore`, 15 for attachments in `sqlstore`. ADR 013.

## What I measured

- A 2 MiB photo adds one op of under 400 bytes; the database stayed under 1 MiB.
- 64 MiB through `Put`: heap growth under 8 MiB.
- Mutation checks, all caught: no hash comparison on receipt; no cap while reading; no refusal of a declared oversize; no sniff; no hash validation in apply; no `mark` (add-wins).

## Honest limits

- No command-line or web way to attach yet (Day 48's web work); the API is tested, the UI is not there.
- Blobs do not sync yet: a peer sees the attachment as "not here yet".
- No deletion, no free-space guard, no removal op (Day 48 / later).
- Older replicas refuse the new op: upgrade all devices first.
- Photos keep their EXIF (including GPS). Decide for the pilot.
- Full test suite, `-race`, lint and wasm results are in the delivery message.

## Your part

1. Upgrade all devices together before anyone attaches a file.
2. Decide whether GPS/EXIF must be stripped before a photo is stored (it affects the privacy section of the pilot paperwork).
3. Open items from before still stand.
