# Good first issues

Small, well-scoped tasks for someone new to the repository. Each one says where
to start and what "done" means. They are real gaps listed in the README's
"Honest limits" or found while building, not invented chores.

`make good-first-issues` (or `scripts/create-good-first-issues.sh`) turns every
`##` section below into a GitHub issue labelled `good first issue`. Run it once,
with the `gh` command logged in. `DRY=1` prints what it would create.

## Run the tests on macOS and tell us what breaks

Everything so far ran on one Linux laptop. The README says so. Nobody has run it
on macOS, and the idle gate (`internal/idle`) reads Linux's `/proc/stat`, so it is
expected to say "never blocks" there.

**Where to start:** `make check`, then `make sim-quick` and `make web-check`.
**Done when:** you have opened an issue or a pull request listing which tests
passed and which failed, with `go version` and `uname -a`. If a test only fails
because of the OS, a fix that skips it with a clear reason is welcome.

## Add `offgrid completion bash`

The command list lives in the `usage` text and a switch in `cmd/offgrid/main.go`.
A user has to read the help to find a subcommand.

**Where to start:** `cmd/offgrid/main.go`, and any small command file such as
`cmd/offgrid/clockcmd.go` for the shape of a command and its test.
**Done when:** `offgrid completion bash` prints a script that completes the top-level
commands, a test checks every command in the switch is in the script (so it cannot
go stale), and the README lists the command.

## Check the dashboard with a keyboard and a screen reader

`docs/day-26.md` says the dashboard was only checked in Chromium and not with a
screen reader or keyboard-only. The buttons are real buttons, but nobody has
tested the whole page.

**Where to start:** `make web-demo SECS=300`, then use only the keyboard.
Page: `internal/web/static/`.
**Done when:** you have listed every thing you could not reach or hear (an
issue is enough), or fixed it in a pull request. Note: the script may only set
`textContent` and a test bans `innerHTML`; keep it that way.

## Stop ignoring the errors from closing and removing files in `internal/perf`

`.golangci.yml` excludes "Close/Remove not checked" because the benchmark
code ignores them. In a benchmark, a failed `Close` on a database could mean a
number was measured on a file that was not fully written.

**Where to start:** `internal/perf/perf.go` (`golangci-lint run` lists them after you
remove the exclusion for that package).
**Done when:** the errors are checked or returned as part of the run (a failed
run is an error, never a quiet number), `make lint` passes, and the perf tests still pass.

## Make `offgrid version` say more

Today it prints `offgrid 0.29.0-day29`. When someone reports a bug we also want
the Go version, the OS and architecture, and the commit it was built from.

**Where to start:** `cmd/offgrid/main.go` (`case "version"`) and `runtime/debug.ReadBuildInfo`.
**Done when:** a plain `offgrid version` stays one line starting with `offgrid X`
(the release workflow checks it), `offgrid version -v` prints the extra lines, and a
test covers both.

## Document the sync protocol in one page

`internal/syncer/protocol.go` defines the wire format and the error codes. There is
no page that tells someone writing a second implementation what to send, in what
order, and what the signature covers.

**Where to start:** `internal/syncer/protocol.go`, `server.go`, `client.go`, and `docs/day-19.md` if you want the reasoning.
**Done when:** `docs/sync-protocol.md` has the endpoints, the headers, how the
signature is computed, every error code with the HTTP status, and one complete
example exchange you produced by running it, not by guessing.
