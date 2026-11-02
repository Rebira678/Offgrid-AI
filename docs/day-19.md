# Day 19: Sync protocol

Until today every replica was an island. This is the day ops start to move: two Offgrid processes, each with its own database, exchange only the ops the other is missing, over plain HTTP, and end up with the same state.

**Done when:** two processes exchange ops over localhost HTTP. They do: `TestTwoOffgridProcessesExchangeOpsOverLocalhostHTTP` builds the real binary and runs a server and a client as separate OS processes, and `make sync-demo` shows it.

## What is in the zip

| File | What it is |
|---|---|
| `internal/store/sqlstore/sync.go` | `Summary`, `OpsSince`, `ApplyRemote`, the errors, and the HLC-ordered fold |
| `internal/store/sqlstore/{rebuild,store}.go` | `Rebuild` and `Verify` now fold in HLC order; new task IDs are `T<n>.<node>` once a replica holds foreign ops |
| `internal/store/sqlstore/sync_test.go`, `store_test.go` | store-level sync tests; the corrupt-log test now timestamps its damaged ops after the real ones |
| `internal/syncer/{protocol,auth,server,client}.go` | wire types, HMAC signing, the HTTP server and the client |
| `internal/syncer/syncer_test.go` | 15 tests: convergence, auth failures, impostors, batching limits, protocol version |
| `internal/task/resolve.go`, `resolve_test.go` | `T5.node-b` style IDs resolve exactly; `T5` resolves only if unambiguous |
| `cmd/offgrid/synccmd.go`, `sync_test.go`, `main.go` | `offgrid sync serve / once / summary`; the two-process test; version `0.19.0-day19` |
| `scripts/sync-demo.sh` | the demo (`make sync-demo`) |
| `README.md`, `Makefile`, `ARCHITECTURE.md` | updated (they overwrite yours: merge by hand if you edited them) |
| `docs/day-19.md`, `docs/day-19-sync.{svg,png}` | this file and the sequence diagram |

## Concepts

**A version summary is one number per node.** Each replica numbers its own ops 1, 2, 3 with no gaps (a rule from Day 6). So "I have everything node B wrote up to 17" is the single number `B: 17`, and a replica's whole knowledge is a small map like `{A: 5, B: 2}`. Two replicas compare maps and know exactly which ops are missing. A round between replicas already in step moves a few hundred bytes (2 requests), however large the logs are.

**Delta exchange.** `POST /sync/ops {have}` returns the ops past `have`; `POST /sync/push {ops}` hands over the ops the server lacks. Batches are 500 ops; `more` says whether to ask again. Ops are returned in HLC order, not node by node. An op can refer to a task another replica created, and the clock guarantees that creation has an earlier stamp, so a receiver never gets an op before the one it depends on, even when a batch ends in the middle.

**Idempotent apply.** Delivering a batch twice, or two overlapping batches, changes nothing:

- An op already held and identical is skipped (counted as a duplicate).
- An op already held with *different content* is a fork: the batch is refused (`409 fork`).
- An op that does not continue its node's sequence (a gap) is refused (`409 gap`), because later ops may depend on the missing one.
- Everything is one SQLite transaction. A refused batch leaves nothing behind.

**The view is a fold in a fixed order.** Before today the view (the `tasks` and `task_notes` tables) was built by replaying ops in the order *this replica* received them. That is fine for one replica and wrong for two: replica A would apply B's ops after its own, replica B the other way round, and they could disagree. Now the view is the op log folded in the order `(timestamp, counter, node, seq)`, which is the same on every replica holding the same ops. If an incoming op sorts after everything already applied, it is applied on top (the common case). If it sorts before something already applied (a concurrent edit made earlier in time), the projection is dropped and refolded inside the same transaction, and the result reports `Rebuilt`.

**IDs.** `T1`, `T2` were per-replica counters. Once a replica holds another replica's ops, new IDs are `T<n>.<node>` (for example `T2.node-b`), which cannot collide. The CLI still accepts `T2` when only one task matches. Two replicas that each created `T1` *before ever syncing* do collide: the sync is refused with `id_collision` and nothing is changed. The fix is to start the second replica from an empty database and let it pull.

**HMAC authentication.** Both sides share a key (`-key-file` or `$OFFGRID_SYNC_KEY`, at least 16 bytes). Every request carries `X-Offgrid-Node`, `X-Offgrid-Time` and `X-Offgrid-Sig` = HMAC-SHA256 over method, path, node, time and the SHA-256 of the body. The server compares in constant time and accepts a time within ±5 minutes. Every *response* is signed too, over the request's signature, the status and the body, so a client knows the peer holds the key and the answer was not changed or replayed from another request. The server only reveals its clock difference to a caller that signed correctly.

## What was checked

- `go test -race ./...` passes; `gofmt` and `go vet` are clean.
- Store level: pull in small batches; double and overlapping delivery; gaps; forks (including whitespace-only JSON differences, which are the same op); 9 kinds of malformed op; far-future clock (nothing written); the local clock moving past a received stamp; three different exchange orders among three replicas giving the same state hash; an older op triggering a rebuild; ID collision; `OpsSince` limits and ordering; and 12 seeded runs of three replicas with skewed clocks doing random creates, updates, notes and partial pulls, then comparing hashes, running `Verify`, and checking that a from-scratch rebuild equals the incremental view.
- HTTP level: unsigned, tampered body, signature reused on another path, changed node name, stale and future requests, the edges of the window (exactly 5 minutes accepted), unknown JSON fields, wrong method, an impostor server with no signature or the wrong key, a replayed genuine response, a response body changed in transit, a different protocol version, same node ID on both sides, and server-side batch caps.
- Process level: a real server process and real client processes on separate databases: a wrong key is refused; round 1 pushes two tasks; both sides edit offline; round 2 pulls 2 and pushes 2; the state hashes are equal; `store verify` passes on both; round 3 moves nothing; the key is not in the server's log.
- Mutation testing on the sync code and the auth code: every mutant that changes behaviour was caught (32 killed). Survivors are equivalent: `<` vs `<=` where two ops can never have equal keys or a node has nothing to send; the early unknown-kind check, which `applyOp` repeats with the same message; and a defensive `max(applied_pos)`.

## Honest limits

- **Localhost only.** Two processes on one computer. No real WiFi, no real phones, no packet loss beyond what Day 11's fault injection already simulated. The client takes an `http.Client`, so the fault transport can be wrapped around it; that is Day 20's job.
- **Not encrypted.** HMAC proves who is talking and that nothing changed; it does not hide the tasks. Use a trusted network or TLS in front. `sync serve` warns when it listens beyond loopback.
- **Clocks matter twice.** The request window is ±5 minutes, so a phone whose clock is off by more cannot sync (the error says by how much; `-window` raises it). And an op stamped more than `-max-drift` (1 hour) ahead is refused, so one badly wrong clock blocks a sync until it is fixed or the limit is raised. A refused batch is refused whole.
- Creating the same title on two replicas at the same time gives two tasks, not one.
- The store still has no op for removing a task or a note (the OR-Set supports it; nothing writes a removal yet). Tombstones and the log are never compacted.
- A concurrent first-sync ID collision is detected, not repaired.
- The pull and the push are two separate transactions on two machines. If a round is cut off, what was applied is kept and the next round continues.

## Try it on your laptop

```bash
make sync-demo                                   # two processes, two databases, one localhost link
go test -race ./internal/syncer ./internal/store/sqlstore
openssl rand -hex 32 > key                       # your own key
bin/offgrid sync serve -db b.db -addr 127.0.0.1:7878 -key-file key &
bin/offgrid sync once  -db a.db -peer http://127.0.0.1:7878 -key-file key
bin/offgrid sync summary -db a.db                # the version summary and the state hash
```

## Post draft: "How two offline phones decide what to send each other"

> Two phones edit the same task list offline. When they meet, what do they send?
>
> Not "everything". Each phone numbers its own edits 1, 2, 3 with no gaps, so its whole knowledge fits in one small map: {phone A: 5, phone B: 2}. They swap maps, and each sends only what the other lacks.
>
> The part that took the thinking: the screen is rebuilt from the edit log in timestamp order, not arrival order, so both phones end up with the same state whatever order edits arrive in. An edit older than something already applied makes the phone refold its view in one transaction.
>
> Every request and response is HMAC-signed. It proves who is talking; it does not encrypt (yet).
>
> Today, two real processes on localhost, state hashes equal after a round. Not real WiFi yet.
>
> Picture: `docs/day-19-sync.png`.

Hooks: engineers who have built sync, local-first and CRDT people.

## Self-check

1. Why is a version summary one number per node, and what rule makes that true?
2. Why does `OpsSince` return ops in timestamp order instead of node by node?
3. What happens when an incoming op sorts before an op already applied?
4. What is the difference between a duplicate, a gap and a fork?
5. What does HMAC protect against, and what does it not?

## Tomorrow: Day 20, two replicas converge (Checkpoint 2)

Both replicas go offline with the chaos switch, edit the same task, add and delete list items, reconnect, and compare state hashes: the split-screen clip.
