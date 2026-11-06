# Day 31: a read must never create a database, and the merge code must stay portable

**Done when:** v1.0.0-beta.1 is tagged with the fix, the product spec is in `docs/`, and CI has a job that builds the merge packages for WebAssembly (green, or a written list of what blocks it). The tag is yours to make; see "Your part" at the bottom.

```bash
go test ./cmd/offgrid -run 'ReadOnlyCommand|WritingCommandsStill'   # the regression test
make wasm            # imports are standard library only, and the merge packages build for WebAssembly
make wasm-test       # also runs their tests as WebAssembly (needs node 18 or newer)
```

## 1. The bug

A command that only looks at a database created one if the file was missing.

```bash
OFFGRID_NODE=node-wrong offgrid sync summary -db tasks.db   # printed "holds: nothing yet", exit 0, and made tasks.db
OFFGRID_NODE=node-real  offgrid exec -db tasks.db '{"intent":"create_task","title":"a"}'
# error: database belongs to a different node: it is "node-wrong", but "node-real" was requested
```

Creating a database is not harmless: it **fixes the replica's node name for good**, and every op ever written from it is tagged with that name. A mistyped path to `sync summary` therefore did not just show an empty list; it could leave a stray replica that later refused the right name. Day 29 hit exactly this (the converge demo ran `sync summary` on a file before the real node had started it) and I worked around it in the script. This is the real fix.

Two commands created the file (`task`, `sync summary`). Four refused, but each with its own wording and exit 1 (`store`, `web`, `suggestions`, `answers`).

## 2. The fix

| Piece | What it does |
|---|---|
| `sqlstore.ErrNoDatabase` | A typed error. `errors.Is(err, sqlstore.ErrNoDatabase)`; the text names the path: `no such database: /path/tasks.db`. |
| `sqlstore.Options{MustExist: true}` | `Open` checks the file first, and also tells SQLite `mode=rw`, so a file deleted after the check is not recreated either. A folder is reported as a folder, not as a missing database. |
| `openExistingStore` (cmd) | The one way a command opens a database it did not come to create. |
| Exit code **3** | The same for every command: `task`, `sync summary`, `store`, `web`, `suggestions`, `answers`. 1 stays "something else went wrong", 2 stays "usage". |

Commands that may create a database still do: `exec`, `agent`, and `sync once/serve/run` (a new replica joins by syncing into a new file).

**Behaviour change to know about:** `store`, `web`, `suggestions` and `answers` used to exit 1 for a missing file. They now exit 3 with the new message. Four existing tests were updated for that (`store_test.go`, `web_test.go`, `suggestions_test.go`, `think_test.go`). If a script of yours tests for exit 1 there, change it to 3.

### The test came first

`cmd/offgrid/nodatabase_test.go` was written and run **before** the fix, and failed for all ten read-only cases, for the sidecar-file case, and for the node-name case (that one reproduces the error above). After the fix it passes. It checks, for each command: exit code 3, the message names the file, nothing on stdout, and **no file or folder left behind** (not `.db`, not `-wal`, not `-shm`).

`internal/store/sqlstore/mustexist_test.go` covers `ErrNoDatabase` itself, that a wrong node name on an existing file is still refused, a folder given as a database, and that the driver honours `mode=rw`.

### Mutation testing

| Mutation | Result |
|---|---|
| Ignore `MustExist` entirely | caught (5 tests) |
| Remove the up-front file check | **survives, and that is correct**: `mode=rw` plus the check after a failed open give the same error. Two layers guard the same thing. |
| Remove `mode=rw` | **survives**: the up-front check hides it. Only a file deleted in the microseconds between the two steps would show the difference, and I could not make a test do that without a hook in production code. `TestDriverModeRWNeverCreates` pins that the driver honours the flag. |

Two surviving mutants, both because the defences overlap. I left them as they are.

## 3. The WebAssembly check

**Why.** A phone that cannot run the Go binary may need to run the same merge rules (the PWA, Days 33 to 36). Two copies of the rules in two languages is how replicas diverge. So `internal/crdt` and `internal/hlc` have to stay portable: standard library only.

`scripts/wasm-check.sh` does three things:

1. **Import guard.** Fails if either package imports anything but the standard library or each other. This is the check that matters, and it is separate on purpose: I tested putting `import _ "github.com/mattn/go-sqlite3"` into `hlc`, and **`GOOS=js GOARCH=wasm go build` still passed**, because with cgo off that driver compiles to a stub that only fails when called. A plain build would have given false comfort.
2. **Build** `GOOS=js GOARCH=wasm go build ./internal/crdt ./internal/hlc`.
3. With `--run`: compile each package's tests to WebAssembly and **run them under node**.

**Result: green.** Both packages already had no outside imports. Their tests pass as WebAssembly too (`crdt` takes about 12 s as wasm against about 3 s natively, in the sandbox).

Mutation-tested: an SQLite import in `hlc` fails the guard; an import of `offgrid/internal/task` fails the guard; a compile error fails the build; the clean tree passes.

**What this does not show.** That the merge code is fast enough on a phone, that the JavaScript side can call it, or that wasm and native give the same *state hashes* for a long random schedule (the tests compare behaviour, not byte-identical hashes across builds). Day 34 is where that gets tested.

**Detail worth knowing.** `wasm_exec` refuses to start when the environment is large, and CI runners have a large one, so the script runs node with a clean environment (`env -i`). Without that, `go test -exec` fails with "total length of command line and environment variables exceeds limit". Go 1.22 keeps the node host script under `misc/wasm`, newer Go under `lib/wasm`; the script tries both. CI runs it on Go 1.22 with node 20.

## 4. CI note: two workflows run the same tests

`.github/workflows/test.yml` (from before Day 28) runs `make test` on every push and pull request, and `ci.yml` already runs the same tests with the race detector on Go 1.22 and stable. Every push runs the suite twice, and the old one is weaker. **I did not delete it** because it is your file; `git rm .github/workflows/test.yml` is safe if you agree (see "Your part").

## 5. Not done, and why

- **"Triage launch feedback"** is yours: I cannot see your LinkedIn, X, GitHub issues or replies. If you paste them here, I will sort them into bugs, docs gaps and feature asks, and turn the bugs into issues.
- **Lint** (`golangci-lint` v2.5.0, your `.golangci.yml`): 0 issues, run on a copy of the code.
- **The product spec's "(set)" numbers** stay open until Day 32 gives measurements from real hardware.
- **Zero-byte database file.** A file that exists but is empty is opened as a database. SQLite in WAL mode really can keep the main file at 0 bytes before its first checkpoint, so treating "empty" as "missing" would be wrong; it is left alone.

## Your part

```bash
git rm .github/workflows/test.yml            # optional: the duplicate workflow
make test && make wasm-test && make lint     # on your machine
git add -A && git commit -m "Day 31: reads never create a database; product spec; wasm check"
git tag -a v1.0.0-beta.1 -m "v1.0.0-beta.1" && git push origin main v1.0.0-beta.1
```
