# Day 6 Master Assessment: Event-Sourced Storage

> **Focus:** Event Sourcing, Immutable Op Logs, Disposable Materialized Views, Deterministic Replay, Schema vs. Projection Versioning & Cryptographic State Hashing  
> **Target Packages:** `internal/store/sqlstore`, `cmd/offgrid` (`storecmd.go`, `exec.go`)

---

## Overview

Day 6 of **Offgrid** transitions the persistence layer from transient in-memory structures to a production-grade, event-sourced SQLite database:  
**"The database stores what happened, not what is true now."**

In standard database design, applications update rows in-place (`UPDATE tasks SET status = 'done'`). The past is permanently overwritten, making multi-device synchronization and audit verification virtually impossible.

Offgrid adopts **Event Sourcing**:
1. Every state mutation is appended as an immutable **operation ("op")** to an append-only `ops` log.
2. The `tasks` and `task_notes` tables are not the source of truth—they are **materialized views** (read caches) computed by replaying the op log.
3. If the view tables are corrupted or dropped (`DROP TABLE tasks`), the database automatically rebuilds them from the op log, byte-for-byte identical, with zero data loss.

---

# Part 1: 20 Conceptual Questions & In-Depth Answers

---

### Q1: What is the fundamental distinction between "Facts" and "State" in distributed systems?
**Answer:**  
- **State:** Represents the current snapshot of reality at an instant in time (e.g., *"Task T1 is done"*). State is mutable, transient, and context-dependent. If two offline nodes report conflicting states (Node A says T1 is open, Node B says T1 is done), neither can determine who modified it, when, or why.
- **Fact:** An immutable record of an event that occurred in the past (e.g., *"At timestamp 1728560000, Node A marked T1 done"*). A fact is permanently true; it can never "un-happen."

By storing facts rather than state, offline nodes can seamlessly synchronize: they simply exchange the missing facts from their respective logs.

---

### Q2: What does "The log is the truth, the tables are a cache" mean in Day 6?
**Answer:**  
In Offgrid:
- The `ops` table (and the minimal `meta` table) contains the **irreplaceable source of truth**. Every business event is recorded here.
- The `tasks` and `task_notes` tables exist solely to provide fast, indexed $O(1)$ and $O(\log N)$ SQL reads for queries like `list_tasks` and `Resolve`.
- Because the tables are derived projections of the log, they can be deleted, truncated, or recreated at any moment without losing a single piece of business data.

---

### Q3: Explain the "Kill-the-View" test and what it proves.
**Answer:**  
To prove that the projection is truly disposable, the test suite (and CLI demo) executes:
```bash
sqlite3 tasks.db 'DROP TABLE tasks; DROP TABLE task_notes;'
```
When the application next opens or runs a command:
1. `ensureProjection()` detects that the tables are missing.
2. It triggers `Rebuild()`, dropping any partial remnants and replaying all ops in sequence from `pos = 1` through `pos = N`.
3. The cryptographic SHA-256 fingerprint (`StateHash`) of the rebuilt view is calculated.
4. The rebuilt hash matches the pre-destruction hash **bit-for-bit**.

This proves that application durability depends entirely on the op log, eliminating fear of view index corruption.

---

### Q4: Why must `applyOp(op)` be a strictly PURE function of the op?
**Answer:**  
A function is **pure** if its return value depends exclusively on its inputs, with zero observable side effects and zero external reads.  
If `applyOp` performed any of the following:
- Read the system clock (`time.Now()`).
- Generated a random or incrementing task ID (`"T" + rand()`).
- Counted rows to decide timestamps.

Then replaying the op log today would produce different timestamps and IDs than when the op was originally written yesterday! Replay would produce divergent database states.  
Therefore, all decisions (the exact task ID `"T3"`, the timestamp, the sequence number) are decided **once at write time** and frozen into the op payload. `applyOp` merely applies the frozen data.

---

### Q5: What is the "Single Code Path Principle" for projections, and why is it vital?
**Answer:**  
In some flawed event-sourced designs, developers write two separate routines:
1. A fast inline updater that mutates tables during live user writes.
2. A batch replay script that rebuilds tables from the event log during disaster recovery.

Over time, developers add features to the live updater but forget to update the replay script. The two diverge, and disaster recovery fails when tested.  
In Offgrid, **`applyOp()` is the ONLY function that ever writes to `tasks` and `task_notes`**. Live writes, disaster `Rebuild()`, and integrity `Verify()` all execute the exact same function. Drift is architecturally impossible.

---

### Q6: How is an op's global identity defined by the tuple `(node_id, seq)`?
**Answer:**  
In a distributed edge network:
- Every replica node possesses a unique identifier (e.g., `node_id = "node-alpha"`).
- Each node maintains its own monotonic sequence counter: `seq = 1, 2, 3...` with **zero gaps**.
- The unique identity of any operation is `(node_id, seq)`.
- A database-level constraint `UNIQUE (node_id, seq)` guarantees that if the same op is received multiple times during a network retry, it is recognized and handled safely.

---

### Q7: What is the local `pos` column in the `ops` table, and how does it differ from `seq`?
**Answer:**  
- **`seq`:** Represents the logical sequence assigned by the originating author node (`node_id`). It is immutable and globally travels with the op across replicas.
- **`pos`:** Represents the local, auto-incrementing insertion order (`pos = 1, 2, 3...`) in **this specific replica's database**.

Because different nodes might receive external sync ops in different arrival orders, their `pos` numbers for the same op may differ, but `(node_id, seq)` remains globally constant.

---

### Q8: What does a gap in a node's `seq` numbers indicate, and how does `Verify()` detect it?
**Answer:**  
Because a node numbers its operations strictly sequentially ($1, 2, 3, \dots, N$):
$$\text{COUNT}(*) = \text{MAX}(seq)$$
In `Store.checkSequences()`:
```sql
SELECT node_id, COUNT(*), MAX(seq) FROM ops GROUP BY node_id;
```
If `COUNT(*)` does not equal `MAX(seq)`, at least one operation is missing. A gap means an op was dropped, corrupted, or deleted. Because subsequent operations may depend on the missing op, the store immediately halts and reports `ErrCorruptLog`.

---

### Q9: How do SQLite triggers enforce the append-only invariant on the `ops` table?
**Answer:**  
In `migrations[0]`:
```sql
CREATE TRIGGER ops_append_only_update BEFORE UPDATE ON ops
BEGIN SELECT RAISE(ABORT, 'ops is append-only: updates are not allowed'); END;

CREATE TRIGGER ops_append_only_delete BEFORE DELETE ON ops
BEGIN SELECT RAISE(ABORT, 'ops is append-only: deletes are not allowed'); END;
```
If any errant application code or rogue query attempts an `UPDATE ops` or `DELETE FROM ops`, SQLite intercepts the query before execution and aborts the transaction with an error.

---

### Q10: What can database triggers protect against, and what can they NOT protect against?
**Answer:**  
- **What they protect against:** Application bugs, ORM accidental deletes, and errant developer SQL scripts executed through the application connection.
- **What they CANNOT protect against:** Someone with OS-level filesystem or root access. An administrator can delete the `.db` file, overwrite bytes using a hex editor, or run `DROP TABLE ops;`. No database-internal trigger can survive the destruction of its containing table.

---

### Q11: Explain the two independent versioning mechanisms: `schema_migrations` vs. `projectionVersion`.
**Answer:**  

| Version Mechanism | Applies To | How Changes Are Handled |
|---|---|---|
| **`schema_migrations`** | Source of truth (`ops`, `meta`). | Uses formal forward migrations (`CREATE TABLE`, `ALTER TABLE`). Each migration runs in its own transaction. Downgrades or newer schemas are rejected (`ErrNewerSchema`). |
| **`projectionVersion`** | Materialized views (`tasks`, `task_notes`). | No data migration scripts! Simply bump `projectionVersion` (e.g., from `"1"` to `"2"`). When `Open()` runs, it drops the old view and rebuilds it instantly from the op log. |

This makes evolving read queries trivial: developers can restructure view tables freely without writing complex data migration scripts.

---

### Q12: What is the cryptographic `StateHash`, and why is canonical serialization required?
**Answer:**  
The `StateHash` is a SHA-256 digest of the entire materialized view:
1. All tasks and notes are read and converted into canonical structs (`canonTask`, `canonNote`).
2. Timestamps are formatted as integer nanoseconds (`int64`), preventing timezone string formatting differences (`+00:00` vs. `Z`).
3. Tasks and notes are sorted in deterministic sequence order.
4. The resulting JSON is hashed: `sha256.Sum256(raw)`.

If two databases produce the identical `StateHash`, their state is mathematically guaranteed to be identical down to the byte.

---

### Q13: How does `Store.Verify()` verify log integrity without mutating the live database?
**Answer:**  
If verification modified the production database, a bug in the verification logic could cause the very corruption it was designed to catch.  
Instead, `Store.Verify()`:
1. Opens an ephemeral in-memory SQLite database: `sql.Open(driverName, ":memory:")`.
2. Creates empty materialized view tables in memory.
3. Replays the live `ops` log from `s.db` into the in-memory scratch database.
4. Computes `ReplayHash` from memory and `LiveHash` from `s.db`.
5. Compares `LiveHash == ReplayHash`. If they differ, it flags `ErrDiverged` without having touched a single row in the live database.

---

### Q14: Why does `decodeStrict()` enforce `DisallowUnknownFields()` on op payloads?
**Answer:**  
Standard JSON parsers silently ignore unrecognized fields.  
If an engineer typos a field name (e.g., `"prority"` instead of `"priority"`), or if an op from a newer application version arrives with modified fields:
- A loose parser would silently ignore the field, setting it to empty and producing incorrect state.
- `decodeStrict()` uses `dec.DisallowUnknownFields()` and `dec.More()`. Any unknown field or trailing data throws an error immediately.
- Corrupted or incompatible payloads fail loudly rather than silently diverging state.

---

### Q15: Why does `applyOp()` refuse unknown op kinds rather than skipping them?
**Answer:**  
In `applyOp()`'s default switch case:
```go
default:
    return bad("unknown kind %q (written by a newer version?)", op.Kind)
```
If the system encounters an unfamiliar event (e.g., `"task.archived"` written by an updated node), skipping it would mean all subsequent operations on that task would execute against an unarchived state. Skipping an unknown event causes silent logical divergence. Failing fast forces operators to update the node software.

---

### Q16: What is Differential Oracle Testing, and how does `sqlstore` use `MemStore` as an oracle?
**Answer:**  
- **The Problem:** SQLite event sourcing involves complex transactions, triggers, and SQL queries where subtle edge-case bugs can hide.
- **The Oracle:** Day 4's `MemStore` is small, simple, and transparently correct.
- **The Test:** In `store_test.go`, a differential test runs **25 seeds $\times$ 200 random operations** (5,000 steps total) simultaneously against `sqlstore` and `MemStore`.
- After every step, the tasks and notes returned by both stores must match identically. Any discrepancy immediately exposes an edge-case bug in the SQL implementation.

---

### Q17: Why are task IDs (`"T1"`, `"T2"`) local to a single replica on Day 6?
**Answer:**  
On Day 6, task IDs are generated simply by counting local task creation ops:
```go
id := fmt.Sprintf("T%d", count+1)
```
In an offline multi-node setup, two disconnected nodes could both create their first task as `"T1"`.  
This is an acknowledged, intentional milestone boundary: Day 6 focuses on single-node event sourcing. Global uniqueness and conflict-free IDs are addressed in the sync milestones (Days 15–20) using Hybrid Logical Clocks and node-prefixed identifiers.

---

### Q18: What is the `execer` interface, and how does it prevent code duplication?
**Answer:**  
In Go's `database/sql` package, `*sql.DB` and `*sql.Tx` share core query methods (`ExecContext`, `QueryContext`, `QueryRowContext`), but neither embeds the other.  
By defining:
```go
type execer interface {
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
    QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
```
Functions like `applyOp`, `listTasks`, and `replay` accept `execer`. They run unmodified inside a live write transaction (`*sql.Tx`), against a scratch in-memory database, or directly against `*sql.DB`.

---

### Q19: Why does SQLite connection pooling use `SetMaxOpenConns(1)`?
**Answer:**  
1. **Single Writer Serialization:** SQLite permits only one write transaction at a time per database file. Multiple concurrent writer connections encounter `busy` or `locked` errors.
2. **In-Memory Database Isolation:** In SQLite, each distinct connection to `":memory:"` creates a completely separate, isolated in-memory database! If the pool opened a second connection, it would connect to a blank, empty database.
3. Setting `SetMaxOpenConns(1)` eliminates locking contention and guarantees that in-memory databases remain stable across queries.

---

### Q20: What are the benchmarked performance characteristics of Day 6 event sourcing?
**Answer:**  
On modern consumer x86_64 hardware (e.g., AMD Ryzen 5 / Intel i5 with SSD):
- **Durable Appends (with fsync / WAL flush):** ~1,100 durable appends per second (~910 $\mu\text{s}$ per transaction).
- **In-Memory Replay Throughput:** ~42,800 ops/second (replaying 10,000 operations in ~0.23 seconds).
- **Large Log Replay:** 50,000 ops replayed in ~1.15 seconds.

Rebuilding views from scratch is fast enough to happen seamlessly at application startup without noticeable user delays.

---

# Part 2: 10 Code-Focused Questions & Line-by-Line Answers

---

### Q21: In `internal/store/sqlstore/ops.go`, examine the `execer` interface.
```go
// From internal/store/sqlstore/ops.go:
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
```
**Answer:**  
1. Abstract interface declaring the three fundamental SQL execution methods.
2. Both `*sql.DB` (database connection pool) and `*sql.Tx` (atomic transaction) implement these exact signatures.
3. This allows database logic (like `applyOp` and `replay`) to execute inside a multi-statement transaction or against a bare connection without duplicating logic.

---

### Q22: In `internal/store/sqlstore/ops.go`, examine `decodeStrict()`. How does it catch malformed JSON?
```go
// From internal/store/sqlstore/ops.go:
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("extra data after the payload")
	}
	return nil
}
```
**Answer:**  
1. Creates a streaming `json.Decoder` over a byte buffer.
2. `dec.DisallowUnknownFields()`: Rejects payloads with misspelled or unexpected JSON keys.
3. `dec.Decode(v)`: Unmarshals JSON into the target struct.
4. `dec.More()`: Verifies that no trailing tokens or garbage bytes exist after the valid JSON object.

---

### Q23: In `internal/store/sqlstore/migrate.go`, examine `migrations[0]`. Walk through the triggers protecting `ops`.
```go
// From internal/store/sqlstore/migrate.go:
CREATE TRIGGER ops_append_only_update BEFORE UPDATE ON ops
BEGIN SELECT RAISE(ABORT, 'ops is append-only: updates are not allowed'); END;

CREATE TRIGGER ops_append_only_delete BEFORE DELETE ON ops
BEGIN SELECT RAISE(ABORT, 'ops is append-only: deletes are not allowed'); END;
```
**Answer:**  
1. `BEFORE UPDATE ON ops`: Fires before any update query touches a row in `ops`.
2. `BEFORE DELETE ON ops`: Fires before any delete query touches a row in `ops`.
3. `RAISE(ABORT, '...')`: Forces SQLite to abort the statement and roll back the enclosing transaction, returning the provided error string to the Go application.
4. This programmatically guarantees that historical events can never be altered or purged through normal SQL queries.

---

### Q24: In `internal/store/sqlstore/migrate.go`, examine `migrate()`. How does it handle newer database schemas?
```go
// From internal/store/sqlstore/migrate.go:
var current int
if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
    return fmt.Errorf("sqlstore: read schema version: %w", err)
}
latest := 0
if len(list) > 0 {
    latest = list[len(list)-1].Version
}
if current > latest {
    return fmt.Errorf("%w (database is at version %d, this program knows up to %d)", ErrNewerSchema, current, latest)
}
```
**Answer:**  
1. Reads the current maximum migration version from `schema_migrations`.
2. Computes the latest version known to this binary (`latest`).
3. If `current > latest`, the database file was written by a newer version of Offgrid.
4. It immediately halts with `ErrNewerSchema` rather than attempting execution, preventing an older binary from corrupting a newer database.

---

### Q25: In `internal/store/sqlstore/store.go`, examine `settleNodeID()`. How does it enforce node identity?
```go
// From internal/store/sqlstore/store.go:
have, ok, err := getMeta(ctx, s.db, "node_id")
if err != nil { return "", err }
switch {
case ok && want != "" && want != have:
    return "", fmt.Errorf("%w: it is %q, but %q was requested", ErrNodeMismatch, have, want)
case ok:
    return have, nil
}
if want == "" {
    var b [6]byte
    rand.Read(b[:])
    want = "n-" + hex.EncodeToString(b[:])
}
if err := setMeta(ctx, s.db, "node_id", want); err != nil { return "", err }
return want, nil
```
**Answer:**  
1. Checks if a `node_id` is already stored in the `meta` table.
2. If already set, and the caller requested a different ID (`want != have`), it refuses to open the database with `ErrNodeMismatch`.
3. If no ID exists: either uses the requested `want`, or cryptographically generates a random ID (e.g., `"n-3f4a9b21"`).
4. Persists the node ID to `meta` so all future sequence numbers belong strictly to this identity.

---

### Q26: In `internal/store/sqlstore/store.go`, examine `Create()`. How does it generate IDs at write time?
```go
// From internal/store/sqlstore/store.go:
var count int
if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops WHERE kind = ?`, KindTaskCreated).Scan(&count); err != nil {
    return err
}
id := fmt.Sprintf("T%d", count+1)
if err := s.record(ctx, tx, KindTaskCreated, createdPayload{ID: id, Title: title, Site: n.Site, Priority: n.Priority}); err != nil {
    return err
}
```
**Answer:**  
1. Executes inside an exclusive write transaction.
2. Queries the total number of `task.created` ops previously recorded in `ops`.
3. Calculates `id := fmt.Sprintf("T%d", count+1)`.
4. Passes `id` inside `createdPayload` to `s.record()`.
5. The ID is permanently encoded into the JSON payload of the op, ensuring that replay will never have to recompute or guess the task's ID.

---

### Q27: In `internal/store/sqlstore/apply.go`, examine `applyOp()` for `KindTaskCreated`.
```go
// From internal/store/sqlstore/apply.go:
_, err := x.ExecContext(ctx, `INSERT INTO tasks
    (id, n, title, title_key, site, site_key, priority, status, created_at, updated_at, done_at)
    VALUES (?, (SELECT COALESCE(MAX(n), 0) + 1 FROM tasks), ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
    p.ID, p.Title, task.TitleKey(p.Title), p.Site, strings.ToLower(p.Site),
    string(p.Priority), string(task.StatusOpen), ts, ts)
```
**Answer:**  
1. Inserts a row into the materialized `tasks` table.
2. `n` (creation order) is calculated via subquery `(SELECT COALESCE(MAX(n), 0) + 1 FROM tasks)`.
3. Stores both raw `title` and folded `title_key` for duplicate detection.
4. Stores both raw `site` and lowercase `site_key`.
5. Initializes `status` as `"open"`, `created_at` and `updated_at` to the op's frozen timestamp `ts`, and `done_at = 0`.

---

### Q28: In `internal/store/sqlstore/apply.go`, examine the default case in `switch op.Kind`.
```go
// From internal/store/sqlstore/apply.go:
default:
    // Refuse, do not skip. A kind this program does not know may change the
    // meaning of everything after it; guessing would silently diverge.
    return bad("unknown kind %q (written by a newer version?)", op.Kind)
```
**Answer:**  
1. If an op possesses an unrecognized kind (e.g., `"task.deleted"` or `"task.assigned"`), the system does **not** ignore it.
2. Ignoring unknown ops would cause subsequent dependent operations to fail or calculate incorrect state.
3. The function returns a `*CorruptOpError` wrapping `ErrCorruptLog`, ensuring execution fails loudly and safely.

---

### Q29: In `internal/store/sqlstore/rebuild.go`, examine `stateHash()`.
```go
// From internal/store/sqlstore/rebuild.go:
for _, t := range tasks {
    c := canonTask{
        ID: t.ID, Title: t.Title, Site: t.Site, Priority: string(t.Priority), Status: string(t.Status),
        Created: nanos(t.CreatedAt), Updated: nanos(t.UpdatedAt), Notes: []canonNote{},
    }
    if !t.DoneAt.IsZero() {
        c.Done = nanos(t.DoneAt)
    }
    for _, n := range t.Notes {
        c.Notes = append(c.Notes, canonNote{Text: n.Text, At: nanos(n.At)})
    }
    canon = append(canon, c)
}
raw, err := json.Marshal(canon)
sum := sha256.Sum256(raw)
return hex.EncodeToString(sum[:]), nil
```
**Answer:**  
1. Reads all tasks from the view in canonical order (`ORDER BY n`).
2. Maps them to `canonTask`, converting timestamps to integer nanoseconds (`nanos(t.CreatedAt)`).
3. Ensures empty slices are initialized (`Notes: []canonNote{}`) rather than `nil` to guarantee consistent JSON serialization (`[]` vs `null`).
4. Marshals to JSON and computes SHA-256 digest, returning a 64-character hex string fingerprint.

---

### Q30: In `internal/store/sqlstore/rebuild.go`, examine `Verify()`.
```go
// From internal/store/sqlstore/rebuild.go:
scratch, err := sql.Open(driverName, dsn(":memory:"))
if err != nil { return rep, err }
defer scratch.Close()
scratch.SetMaxOpenConns(1)
if err := resetProjection(ctx, scratch); err != nil { return rep, err }

n, _, err := replay(ctx, scratch, s.db, 0)
if err != nil { return rep, fmt.Errorf("sqlstore: verify: %w", err) }

rep.ReplayHash, _ = stateHash(ctx, scratch)
rep.LiveHash, _ = stateHash(ctx, s.db)
if rep.LiveHash != rep.ReplayHash {
    return rep, fmt.Errorf("%w (live %s, replayed %s)", ErrDiverged, rep.LiveHash[:12], rep.ReplayHash[:12])
}
return rep, nil
```
**Answer:**  
1. Opens an independent, isolated `:memory:` database with `SetMaxOpenConns(1)`.
2. Creates clean projection tables via `resetProjection()`.
3. Calls `replay()` to apply all ops from `s.db` into `scratch`.
4. Computes `ReplayHash` from scratch and `LiveHash` from the production database.
5. If hashes mismatch, returns `ErrDiverged`, definitively identifying tampering or projection bugs without mutating production data.
