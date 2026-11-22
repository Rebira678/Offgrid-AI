# ADR 013: Attachments are blobs named by hash; the log carries only a reference

**Status:** accepted, 2026-11-21 (Day 47 of the Days 31 to 60 plan)

## Context

A photo of the fault decides whether a work order is taken seriously, so tasks need files. The op log is replicated whole, verified end to end, hash-chained and kept forever. A photo is 1 to 5 MB, a task has a handful, and a log with photos in it would make every catch-up, every `Verify` and every backup carry them.

## Decision

1. **Bytes live outside the log**, in `internal/blobstore`: one file per SHA-256, at `<dir>/ab/cdef...`, default `<database>.blobs/`. The log gets one `attachment.added` op: `{task, hash, size, type, name?}` (about 250 bytes).
2. **Content addressing.** The name is the checksum. The same photo added twice, or received from two peers, is stored once. Bytes from a peer are accepted only if they hash to the hash the (signed) op names (`PutExpected`); a mismatch keeps nothing, and a damaged stored copy is replaced rather than trusted.
3. **Streaming, atomic.** Bytes go through the hasher into a temp file, are fsynced, then renamed into place and the directory fsynced. A crash leaves no blob or a whole blob. Leftover temp files are swept on open. Nothing is held in memory (32 KiB buffer; a test moves 64 MiB with under 8 MiB heap growth).
4. **Size cap**: `Options.MaxBlobBytes`, default 25 MiB. A declared size over it is refused before any byte is read; an undeclared or lying size stops at cap+1 bytes. Separately an op may not declare more than 256 MiB, so an op cannot describe an absurd file even when this replica's cap is higher than a peer's.
5. **Types are an allowlist** (JPEG, PNG, WebP, GIF, HEIC/HEIF, PDF, plain text, MP4). No SVG or HTML: a stored file may be served to a browser. The first bytes must look like the declared type (a sanity check, not a boundary; files are to be served only as the declared type with sniffing off, Day 48). Names are display text only: at most 120 characters, no control characters or path separators; the file on disk is never named by it.
6. **The op can arrive before the bytes.** Applying the op never touches the disk, so replay, rebuild and `Verify` do not depend on the blob directory, and two replicas have the same state fingerprint while one is missing a file. `Attachment.Present` says whether the bytes are here. Fetching missing blobs is Day 48.
7. **Order of writes:** blob first, op second. An acknowledged attachment always has its bytes; a crash between the two leaves an unreferenced blob, which is harmless and collected on Day 48.
8. **CRDT semantics:** adding an attachment counts as an edit of the task (add-wins, like a note): an attachment made without having seen a concurrent delete keeps the task alive. An attachment on a task that does not exist is inert when the task is void (revoked author), like any other op.
9. The projection gets an `attachments` table (projection version 5, rebuilt automatically on open from the log). The state fingerprint includes attachments only when there are any, so databases without attachments keep their old fingerprints.

## Costs and limits

- **Mixed versions:** an older replica refuses `attachment.added` as an unknown op kind. As with Day 41, upgrade all devices before anyone attaches a file.
- **Disk growth** is the blob directory, not the log. There is no free-space guard, reference counting or deletion yet (Day 48), and **no op removes an attachment**.
- **Local limit of 20 attachments per task** is enforced when writing, not when applying: the fold must give every replica the same view.
- **Not encrypted at rest** (same as the database; the directory is mode 0700, files 0600).
- A directory belongs to one process.
- No thumbnails, no image re-encoding, no metadata (EXIF) stripping: a phone photo carries its GPS position. Stripping is a decision for the pilot to make, not done here.

## Rejected

- **Blobs in SQLite**: grows the database that is checkpointed, vacuumed and copied for backups, and a 5 MB write holds the single writer.
- **Blobs in the op log**: see Context.
- **Naming files by task or upload name**: no dedupe, path-traversal surface, and no checksum.
- **Trusting the size or type a peer states**: both are re-checked against the bytes.
