# Day 37: Security Checkpoint 1

**Done when** (Security Checkpoint 1 criteria), and current status:

| Criterion | State |
|---|---|
| Default Operation Signing | Done: Auto-provisions mode-0600 ECDSA P-256 keys on first write; all 9 CRDT op kinds signed |
| Deprecating Shared Sync Key | Done: Peer identification header `X-Offgrid-Device-ID` sent over wire; per-device signatures enforce authenticity |
| Signed Sync by Default | Done: `sync serve`, `sync once`, and `sync run` enforce signed sync by default; `-allow-unsigned` permits legacy Day 30 migrations |
| Wire Protocol Round-Trip | Done: `WireOp.Sig` preserves device signatures across HTTP sync rounds without dropping signatures |
| TOFU Device Binding | Done: Trust-on-first-use binds replica to authoring device ID; subsequent key mismatches rejected with `ErrKeyChange` |
| Security Demo & Audit | Done: `scripts/checkpoint1-security-demo.sh` verifies automated key provisioning, sync, and tamper detection |
| Full Test Suite Green Under `-race` | Done: All 32 Go packages pass, WASM builds, golangci-lint reports 0 issues |

---

## 1. Architectural Changes

### Default Operation Signing
In Day 36, operation signing was opt-in: commands required an existing key file or wrote unsigned operations. In Day 37 (Security Checkpoint 1), operation signing is the default:
- `deviceSigner()`: If no key exists at the default path (`~/.config/offgrid/device.key`), `identity.LoadOrCreate` automatically generates a mode-0600 ECDSA P-256 key pair on first write.
- Read-only operations (`store verify`, `store head`, `store info`) use `deviceSignerReadOnly()`, which never creates a file on disk.
- Unsigned writes are only permitted when explicitly requested via `OFFGRID_ALLOW_UNSIGNED=1` (for legacy migration testing).

### Wire Protocol & Peer Identification
- Defined `HeaderDeviceID = "X-Offgrid-Device-ID"`.
- `syncer.Client` and `syncer.Server` exchange their device IDs on every HTTP request and response.
- `WireOp` includes `Sig *sqlstore.OpSig` with `json:"sig,omitempty"`. `ToWire` and `FromWire` preserve signatures during transit so receivers verify incoming ops before applying them.

### Enforced Signed Sync & Legacy Transition
- Sync commands (`sync serve`, `sync once`, `sync run`) require signed operations by default when a device key is present.
- A new flag `-allow-unsigned` allows operators to sync with legacy Day 30 databases that have not yet been attested.
- If an operator passes `-require-signed` without a device key, the CLI exits immediately with actionable instructions (`offgrid identity init`).

---

## 2. Threat Model Mitigations

| Threat | Name | Before Day 37 | Day 37 Resolution | Status |
|---|---|---|---|---|
| **N2** | Spoofing | Any holder of the shared HMAC key could forge ops under any node ID | Shared key deprecated; peer device ID header sent; each op verified against author's device key via TOFU | **mitigated** |
| **N7** | Repudiation | Signing was opt-in; unsigned ops could not be attributed to a device | Signing default on all writes; `store verify` names authoring device; unsigned-after-signed disallowed | **mitigated** |

---

## 3. Benchmarks & Performance Verification

Measured on AMD Ryzen / Linux (6 cores / 12 threads) and Sandbox:

| Benchmark | Sandbox (2 vCPU) | Local Dev (12 vCPU) | Raspberry Pi 4 |
|---|---|---|---|
| Sign one op (ECDSA P-256) | 31.0 µs | 33.8 µs | _measured Day 36_ |
| Verify one op (one core) | 66.0 µs | 72.4 µs | _measured Day 36_ |
| Verify 1,000 ops (`verifyPool`) | 45.6 ms | 16.2 ms | _measured Day 36_ |
| Local write (signed vs unsigned) | +21.0% | +6.5% | _measured Day 36_ |
| Apply 1,000 remote ops (signed) | 120.4 ms | 124.8 ms | _measured Day 36_ |

---

## 4. Verification Checklist

1. **Unit & Edge Tests**:
   - `internal/store/sqlstore/day37_checkpoint_test.go`: All 9 CRDT op kinds verified signed, TOFU binding enforcement, tamper detection across payload/seq/sig, parallel `verifyPool` stress test with 120 jobs, attestation migration integrity.
   - `cmd/offgrid/day37_cli_test.go`: CLI auto-provisioning, 0600 file permissions, `X-Offgrid-Device-ID` wire header exchange, `-allow-unsigned` legacy sync.
2. **Automated Demonstration Script**:
   - `scripts/checkpoint1-security-demo.sh`: Demonstrates key auto-provisioning, sync between two field nodes, tamper detection, and verification.
3. **CI Pipeline Compatibility**:
   - `go test -race ./...` green across all 32 packages.
   - `make wasm` builds cleanly.
   - `golangci-lint` passes with 0 issues.
