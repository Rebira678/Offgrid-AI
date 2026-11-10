# Day 34: a key per device, and the security baseline

**Done when** (the roadmap's list), and where each stands:

| Criterion | State |
|---|---|
| Key survives restart | Done and tested (`TestKeySurvivesRestartAndIDIsStable`; three fresh loads, an old signature still verifies, a new one from the reloaded key verifies against the original public key) |
| Wrong permissions refuse to start | Done for the identity commands and `LoadOrCreate`. **Nothing in the node reads the key yet**, so `sync serve` cannot refuse on it until Day 36 wires it in |
| Device ID is stable | Done and tested, with an independent vector (the P-256 generator point, hashed with Python) |
| No key bytes in any log | Done and tested (below) |
| Fuzzers clean for 10 minutes | **Not shown here.** Each target ran 20 to 60 seconds in my sandbox, clean. The 10-minute run is the nightly workflow's job, or yours: `make fuzz-smoke FUZZTIME=10m` |
| `govulncheck` green | **Not run.** My sandbox cannot reach the vulnerability database. The job is written; the first run is yours |

## 1. The identity

```bash
offgrid identity init            # once per device; never replaces a key
offgrid identity show [-json]    # changes nothing; with no key it exits 3
```

The key lives at `-key FILE`, else `$OFFGRID_IDENTITY_KEY`, else the config folder (`~/.config/offgrid/device.key` on Linux).

**Decision to check: `show` never creates the key.** The roadmap says "generate on first run". Day 31 taught that a command which only looks must not create things, and a key is worse than a database: a stray one is a second identity. So generation is the explicit `identity init`, and Day 36's commands that need a key will call `LoadOrCreate` only where you say so. If you would rather it appeared on first use, it is one line.

| Piece | Choice and why |
|---|---|
| Key | ECDSA P-256, `crypto/ecdsa` and `crypto/rand` only. One per **device**, not per person: attribution is to a device with a human label from enrollment (Day 38). Cost: a lost phone means revoking a key |
| File | One line: `offgrid-key v1 ecdsa-p256 <base64url PKCS#8>`. The **algorithm tag is in the file and in every public key** (`ecdsa-p256:<base64url>`), so a later algorithm is a new tag and old data still reads. A file with an unknown version or algorithm is `ErrUnsupported` ("may come from a newer Offgrid"), never "corrupt" |
| Device ID | `dev_` and the first 32 hex characters (128 bits) of the SHA-256 of the 65-byte public key. It cannot disagree with the key. The Day 33 phone page hashes the same bytes, so a phone and a node will agree |
| Signatures | SHA-256 then ECDSA, as the 64-byte raw `r\|\|s` form that WebCrypto produces. The Go side verifies what the browser side signs |
| Permissions | Mode 600 required (400 also accepted). Checked on the **opened file's** mode, so a symlink to a readable file is refused. The folder is made 700. `chmod 600 FILE` is in the error |
| Never replaced | A missing file may be created. A wrongly protected, corrupt or unsupported file is an error: a silent new key would change the device's identity |
| Creation | Written whole to a private temporary file, synced, then **hard-linked** into place: a reader never sees half a key, and when 16 processes start at once exactly one creates it (tested) |
| Printing | `Identity` prints as `Identity{ID: dev_..., key: [redacted]}` under every `fmt` verb, logs as ID and algorithm under `slog`, and marshals to the public facts only |

## 2. No key bytes in any log

`TestNoKeyMaterialInAnyOutput` prints the identity with eight `fmt` verbs (value, pointer, in a slice), through `slog` text and JSON, with `json.Marshal` (bare and inside another struct), and collects the error from a wrongly protected file and from a refused overwrite. It then searches all of it for the key in the encodings it could leak as: the file's own base64, standard and URL base64 of the DER, hex, decimal and base-16 of the private scalar. `TestIdentityCommandNeverPrintsTheKey` does the same for everything the command prints on success and on four failure paths. Corrupt-file tests also check that **the error does not repeat the file's content**.

Honest limit: the search proves the paths I thought of. A new log line added later is not covered unless it goes through these types. Memory is not zeroed.

## 3. Fuzz targets

| Target | What a stranger's bytes can reach | The property |
|---|---|---|
| `FuzzApplyRemoteOp` (`sqlstore`) | the op decoder: any kind, node, sequence, time, counter and payload through `ApplyRemote` into a real store | no panic; a refusal is always a **typed** error (an untyped one would be a 500 on the wire); a refused op changes nothing; `Verify` (hash chain and replayed view) still passes |
| `FuzzSyncRequestBody` (`syncer`) | a correctly signed request with any body on any method and path | no 5xx; never 401 for a valid signed request; every answer is JSON; a 2xx carries a signature that verifies; a 400 carries the `bad_request` body |
| `FuzzSyncAuth` (`syncer`) | any method, path, node, time, signature and body, with an option to re-sign so the fuzzer reaches the time check | 2xx **only** for the right MAC over exactly those bytes and a fresh time; and a request with a valid MAC and a fresh time is never refused |
| `FuzzParseKeyFile` / `FuzzDeviceID` (`identity`) | the key file parser and the public-key parser | no panic; errors are typed; anything that parses is a usable key that survives re-encoding; an all-zero signature never verifies |

Seed corpora are in the tests (`f.Add`), so they run on **every** `go test`, not only when fuzzing. Nightly: `.github/workflows/fuzz.yml`, ten minutes per target, in parallel jobs, with the learned corpus cached between nights; a crash is uploaded, and copying it into `testdata/fuzz/<Target>/` turns it into a regression test.

**What they found: nothing**, in 20 to 60 seconds each (about 36,000 executions of the op target, 890,000 and 600,000 of the two sync targets). That is a statement about those minutes, not a proof. The 10-minute run is still to be done.

### Mutation testing (do the tests catch a real defect?)

I broke the code on purpose and checked that the tests failed. The honest tally:

- **identity**: 22 mutations (permission mask, overwrite, silent replace, printing the key through `String`, `LogValue` and `MarshalJSON`, ID derivation, modes, signature length, curve check, error echo, symlink). Five first survived: three equivalent (the two silent-replace mutants are stopped a second time by `Create` refusing to overwrite, and the size limit is also covered by the one-line check), one **real gap** (`MarshalJSON` leaking), which I closed with a test and re-ran, and one mutation of mine that changed nothing, which I redid. After that every non-equivalent mutant is caught.
- **command**: 6 of 6 caught (show creating the key, the exit code for "no key", failures exiting 0, the flag not beating the environment, init lying about creating, usage exit codes).
- **op fuzz target**: 5 tried. Caught: an untyped apply error, an untyped gap error, and removing the rollback (which hung the run). Two survived and are equivalent: skipping the JSON validity check (`json.Compact` rejects the same input one step later), and the `Compact` error path itself (unreachable behind the first check).
- **sync fuzz targets**: the first round **missed the time window entirely** (3 survivors: the window not enforced, one-sided, and an empty signature). The empty signature is equivalent (the compare fails anyway). The window was a real gap: my seeds shifted the time without re-signing, so they were refused for the wrong reason. I added the re-sign option and a second direction to the oracle (a valid fresh request must not be refused), after which the window mutants, including an off-by-one at exactly 300 s, are caught. Final: every non-equivalent mutant caught (13 of 13).

One mutant I ran **hung the test run** (removing the rollback leaves the single SQLite connection locked). A hang is a failure, not a pass.

## 4. `SECURITY.md` and CI

- `SECURITY.md`: how to report privately, supported versions, what is protected and where each protection stops, what is not protected yet, what is tested, how secrets are handled. It copies the README's "Honest limits" rather than softening them.
- `.github/workflows/govulncheck.yml`: on push, pull request, **weekly** (an advisory can appear with no code change) and by hand; Go `stable`, `govulncheck@v1.1.4`.
- `.github/workflows/fuzz.yml`: nightly, described above.
- `make fuzz-smoke [FUZZTIME=10m]` and `make vuln` run the same things locally.

## 5. Things I noticed and did not change

1. **The sync key file's permissions are not checked**, while the new identity key's are. `LoadKey` reads any readable file. It is the secret every replica shares, so it deserves the same rule; it is a small change for Day 36, which touches that path anyway. `SECURITY.md` says so.
2. **`.gitignore` has nothing for keys.** `ca.key`, `node.key` and `device.key` could be committed by accident (you already commit `spike-results.jsonl`). Add `*.key`.
3. **The identity key is not used by anything yet.** Today the key is a fact on disk. Day 36 uses it.
4. **Windows:** the permission check is skipped there (the mode bits mean nothing), and `os.Link` is assumed to work on the filesystem, with a rename fallback that has a tiny race. I did not run on Windows.
5. **Trailing data after a request body is accepted** by the sync request decoder (`{"ops":[]} junk` is a valid push), while the op payload decoder rejects it. It is authenticated input and harmless, but inconsistent; I left it.

## Your part

1. Merge into your `main.go`: next to `case "ca":` add `case "identity": return cmdIdentity(args[1:], stdout, stderr)`, and one usage line (below).
2. `make test && make wasm-test && make lint`, then `make fuzz-smoke FUZZTIME=10m` (50 minutes, or let the nightly do it).
3. Add `*.key` to `.gitignore`. Enable **Settings, Code security, Private vulnerability reporting** on the repository, or `SECURITY.md` points at a button that is not there. Check the contact line and the one-week response aim are ones you will keep.
4. `offgrid identity init` on the laptop and the desktop; paste the two device IDs into the real-device report.
5. Push and watch the `govulncheck` job's first run: a finding is information, not a failure of the day.
