# Contributing to Offgrid

Thank you for looking. Offgrid is an offline-first AI agent runtime in Go: local
models, SQLite, a custom op-log CRDT and signed sync. It is built in public, one
small step a day, and it is meant to be read as much as run.

The project's rule is **honest numbers and stated limits**. A change that makes
a claim (faster, safer, more correct) needs the test or measurement that shows it.

## Set up (about 5 minutes)

You need Go 1.22 or newer and a C compiler (the SQLite driver uses cgo):

```bash
sudo apt install build-essential      # Debian / Ubuntu / Zorin; use your system's equivalent
git clone <this repository> && cd offgrid
go get pgregory.net/rapid@v1.2.0      # only if go.mod does not list it yet (property tests)
make check                            # vet + gofmt + every test
```

You do **not** need Ollama, a GPU or a network for the tests. Anything that talks
to a model uses a stand-in in tests. To try the real thing you need
[Ollama](https://ollama.com) and a small model (`make models`).

## Before you open a pull request

```bash
make check        # vet, gofmt, tests
make test-race    # the race detector
make lint         # golangci-lint v2, config in .golangci.yml
```

CI runs the same things on Go 1.22 and the current release. If CI fails, the
log says which step; fix that step locally with the matching command above.

## What a good change looks like

- **Small and focused.** One idea per pull request. Say what it does and why.
- **A test that fails without your change.** For a bug, first write the test that
  reproduces it. For sync, store or merge code, a property test (`rapid`) or the
  simulator (`make sim-quick`) is usually the right tool.
- **Existing tests keep passing,** including the ones that look slow.
- **gofmt, and no new dependencies** unless you have asked first. The binary is
  meant to stay small and to build with `go build`.
- **No network at runtime by default.** Nothing may contact the internet unless
  the person using it configured that, and says so in the docs.
- **Update the docs your change touches:** the README's "Honest limits" if you
  add or remove one, `ARCHITECTURE.md` if you change a guarantee.

## Where things live

| You want to change... | Look in |
|---|---|
| the commands and their flags | `cmd/offgrid/` |
| tasks, the log, the SQLite store, hashing | `internal/store/sqlstore/`, `internal/task/` |
| sync between devices | `internal/syncer/` |
| the merge rules | `internal/crdt/`, `internal/hlc/` |
| the model, routing, the queue | `internal/agent/`, `internal/router/`, `internal/jobqueue/`, `internal/drain/` |
| the dashboard | `internal/web/` (page in `internal/web/static/`) |
| benchmarks | `internal/perf/` (`make bench`) |
| a day's reasoning | `docs/day-NN.md` |

`ARCHITECTURE.md` lists each guarantee with the test that enforces it. Start there.

## Good first issues

[docs/good-first-issues.md](docs/good-first-issues.md) lists small, well-scoped
tasks, each with where to start and what "done" means. Comment on the issue before
you start so two people do not do the same thing.

## Reporting a bug

Use the bug report template. The most useful bug report has the command you ran,
the output, `offgrid version`, your OS and CPU, and whether Ollama was running.
For a sync or store bug, `offgrid store verify` output helps a lot. **Do not
attach your real database** without looking at it first: it holds your tasks.

## Security

If you find a way to read or change someone's data without access (the dashboard,
the sync protocol, the op log), please do not open a public issue. Contact the
maintainer privately through the email on the GitHub profile, and allow a
reasonable time to fix it. The known limits are written in the README
("Honest limits"): sync is signed but not encrypted, and the hash chain detects
edits but does not identify who made them.

## Licence

By contributing you agree that your contribution is licensed under the
[MIT License](LICENSE), the same as the project.
