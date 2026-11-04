# Day 28: a repository that looks (and is) maintained

**Done when:** CI is green and a release is published. **Both of those need your GitHub account**, so they are the checklist below, not something I can claim. What I could do, and did, is run every step of both workflows on my machine.

## What is in the zip

| File | What it is |
|---|---|
| `.github/workflows/ci.yml` | on every push to `main` and every pull request: `go.mod` is tidy, build, vet, gofmt, tests **with the race detector** on Go 1.22 (what `go.mod` promises) and on the current release; golangci-lint; and a smoke job that runs the built binary, `make web-check` and `make tamper-demo` |
| `.github/workflows/release.yml` | on a tag `v*`: tests, then builds `linux/amd64` and `linux/arm64` (each on its own kind of machine, no cross compiler), checks the binary prints the tag's version, packs `.tar.gz`, writes `SHA256SUMS`, and publishes the release. Only the last job can write |
| `.golangci.yml` | the lint set, with each exclusion and its reason written down |
| `LICENSE` | MIT, "Copyright (c) 2026 Rebira". **Your decision**; change it before you push if you want something else |
| `CONTRIBUTING.md`, `.github/ISSUE_TEMPLATE/`, `.github/pull_request_template.md` | how to set up, what a good change is, where things live, how to report a bug, a private route for security reports |
| `docs/good-first-issues.md`, `scripts/create-good-first-issues.sh` | six real starter tasks; one command opens them as GitHub issues (`DRY=1` to preview) |
| `Makefile`, `README.md`, `.gitignore` | `make lint`, `make release-local`, `make good-first-issues`; badges, an Install section, a licence section; `dist/` ignored |

The version is now stamped at build time (`-X main.version=...`), so a release binary says the tag it came from. A plain `go build` still says its day version.

## What linting found

66 findings on the first run. Real ones, fixed:

- **`t.Chdir` in a test needs Go 1.24, and `go.mod` says 1.22.** CI on Go 1.22 would not have compiled that test package. It now uses `os.Chdir` with a cleanup.
- 3 assignments whose value was never used (in tests), 1 ignored `json.Unmarshal` error (now explicit, with the reason).

Switched off, with the reason in `.golangci.yml`: style-only staticcheck suggestions (they change no behaviour), errors from `Close`/`Remove` and from writes to a terminal or response, unchecked errors in tests. One of those is a real follow-up, and it is a good-first-issue: `internal/perf` ignores `Close` errors, and in a benchmark a failed close could mean a number measured on a file that was not fully written.

## What I ran, and what I could not

Ran here (Go 1.24, Linux x86-64):

- `go vet`, `gofmt -l`, `go test ./...` and `go test -race ./...`: all pass (906 test functions, 28 packages).
- `golangci-lint` v2.5.0 with the committed config: 0 issues.
- `make web-check` and `make tamper-demo`: pass.
- The release build command, character for character: `go build -trimpath -ldflags "-s -w -X main.version=9.9.9"`, and the binary printed `offgrid 9.9.9`.
- Both workflow files parse as YAML; the issue script's dry run lists the six issues.

**Not run, because I cannot:**

- **GitHub Actions itself.** No YAML check replaces a first run. If a step fails, the log names it. Likeliest first failures: the `go.mod` tidy step if `rapid` is not committed yet; the lint action's version; the arm64 runner label.
- **Go 1.22.** I have only 1.24 here. I removed the one 1.24-only call the linter found, and searched for the other newer APIs I know of, but the Go 1.22 job is where a miss would show. It is `fail-fast: false`, so the other job still reports.
- **The arm64 binary** has never been built or run by me.
- **Any other distribution than the one I'm on.** The binary needs glibc 2.35+ (built on Ubuntu 22.04).

## The post (Week 4 recap)

Attach a screenshot of the green CI run and the release page, **after they exist**.

> Week 4 of building an offline AI runtime in public. 30 days, one Go binary.
>
> This week: reproducible benchmarks (`make bench`), a dashboard inside the binary, and a log that notices when someone edits it by hand. Then today, the boring part that makes it real: CI on every push (build, vet, race-detector tests, lint), release binaries for Linux with checksums, a licence, contribution notes, and six starter issues.
>
> The linter found a real bug before anyone else did: one test used a Go 1.24 function while the project promises 1.22.
>
> What it still hasn't done: run on a second machine or on a real network. That's the next thing I need help with.

Hook for recruiters: *a project that looks maintained, with the receipts.* If you want help, the macOS issue is the one I most want.

## Checklist (in this order)

- [ ] Decide the licence. MIT is in the zip; if you want Apache-2.0 or something else, replace `LICENSE` and the badge in the README.
- [ ] `go get pgregory.net/rapid@v1.2.0`, then `go mod tidy`, and commit `go.mod` and `go.sum`. CI's first step fails without this.
- [ ] Unzip over the repo, `make check` and `make lint` (install golangci-lint v2 once), then commit and push to `main`.
- [ ] Open the Actions tab. Fix whatever is red (send me the log and I will help). **Done when the `ci` workflow is green.**
- [ ] Settings, Actions, General: make sure GitHub Actions is allowed to run.
- [ ] Tag and push: `git tag -a v0.28.0 -m "v0.28.0" && git push origin v0.28.0`. **Done when the release has two `.tar.gz` files and `SHA256SUMS`.** Download one and run `./offgrid version`.
- [ ] `gh auth login`, then `make good-first-issues` (try `DRY=1` first).
- [ ] Add a repo description and topics (`go`, `offline-first`, `crdt`, `local-llm`, `sqlite`).
- [ ] Still open from before: `make bench` on the EliteBook, the Day 26 and 27 clips, the 7-8B row, `ollama --version`.
- [ ] 15 minutes of outreach.
