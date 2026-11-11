# Security policy

Offgrid is a pre-1.0 beta kept by one person. This file says what is protected today, what is not, and how to report a problem.

## Reporting a vulnerability

Please do not open a public issue for anything that lets someone read or change data without access (the sync protocol, the op log, the dashboard, the key handling, the private CA).

- Use GitHub's **Security, then "Report a vulnerability"** on this repository (private vulnerability reporting), or
- write to the maintainer at the email on the GitHub profile.

Please include `offgrid version`, what you did, what you expected and what happened. A proof of concept helps; a real database does not, so please do not attach one.

What to expect: I aim to answer within a week, to tell you whether I think it is a vulnerability, and to fix or document it before it is made public. I will credit you unless you prefer not to be named. This is one person's best effort, not a service level.

## Supported versions

Only the latest `1.0.0-beta.N` release and `main`. There are no backports.

## What is protected today

| Asset | Protection | Where it stops |
|---|---|---|
| Sync requests and responses | HMAC-SHA-256 over method, path, node, time and body with a shared key; five-minute window; constant-time compare | The key is shared by every replica. The ops cross the wire readable (transport encryption is Day 40) |
| The op log | A hash chain per replica plus individual ECDSA P-256 signatures per op (`op_sigs`); `offgrid store verify` recomputes the chain and checks all signatures | It catches forged edits, unauthenticated chain rewrites, and fork rewrites. Legacy pre-signing history is anchored via genesis attestations |
| Incoming ops | Validated before they touch the database; cryptographic signature verified against tagged public key in parallel; refused ops change nothing | A valid-looking op with a valid signature from an enrolled device key is accepted |
| The dashboard | Binds to localhost only | Not for use over a network |
| The device key (Day 34-36) | ECDSA P-256, one per device, mode 600 (refused otherwise), never printed or logged, not extractable through any Offgrid command | The file is not encrypted at rest: anyone who can read your files can read it. Windows file permissions are not checked |
| The site network certificate (Day 33) | A private CA limited by name to private addresses and `.local`, `.lan`, `.offgrid`; the CA key is meant to stay off the node | A phone that installs the CA trusts it for those names only, but the installer must still keep `ca.key` safe |

## Not protected yet (the roadmap says when)

- **Encryption on the wire.** Sync is signed, not encrypted. Use a trusted network until the mutual-TLS transport (Day 40).
- **Revocation.** (Day 39.) A lost phone cannot yet be cut off; today you change the shared sync key on every replica.
- **Data at rest.** The database and the key files are plain files.
- **A compromised node.** Anything with write access to a replica can write ops that replica then syncs.

## What is tested for

- `go test -race ./...`, `golangci-lint` and a WebAssembly build of the merge packages on every push.
- **Fuzzing** (Go's `go test -fuzz`): the incoming-op path (`ApplyRemote`), the sync request parser and authentication, and the key file parser. The seed corpora run as ordinary tests on every push; each target is fuzzed for ten minutes every night (`.github/workflows/fuzz.yml`). A crash is kept as a regression test.
- **`govulncheck`** on every push and once a week (`.github/workflows/govulncheck.yml`), so a new vulnerability in a dependency or the standard library shows up even when no code changed.
- Key handling has tests for the properties that matter: the key survives a restart, a wrongly protected key file refuses to load, the device ID is stable and derived from the public key, and no private-key bytes appear in any output, error or log line.

## Handling of secrets

- **Device key:** `offgrid identity init` makes it; the file is one line, mode 600, in a folder of mode 700. Back it up privately. Never commit it (`*.key` belongs in `.gitignore`).
- **CA key:** keep `ca.key` on removable media, not on the node. The node needs only `node.crt`, `node.key` and `ca.crt`.
- **Sync key:** a file of at least 16 random bytes, shared between replicas. Kept mode 0600 or 0400: enforced by `syncer.LoadKey` on non-Windows (symlinks and open permissions rejected).
- Offgrid never logs key material. If you find a place where it does, that is a vulnerability: please report it.
