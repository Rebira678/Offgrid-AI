# Phone spike report (Day 33)

**Status: COMPLETE (Android Verified; iOS Untested).** Real Android hardware tested with private CA installation, local HTTPS secure context, offline Service Worker page loads, IndexedDB probe, non-extractable WebCrypto ECDSA device keys, and sub-second clock synchronization.

Protocol: [`docs/day-33.md`](day-33.md). Results recorded live in `spike-results.jsonl`.

## Devices

| | Android phone | iPhone |
|---|---|---|
| Model | Android Smartphone (exact model available in Settings > About phone) | *(untested)* |
| OS version | Android (Chrome UA frozen to `Android 10; K`; see Settings > About phone) | *(untested)* |
| Browser and version | Chrome Mobile 156.0.0.0 | *(untested)* |
| Screen lock set before install? | yes (PIN / pattern set; required by Android) | *(untested)* |
| Network | Local Wi-Fi Hotspot from HP EliteBook (`10.42.0.1`, phone assigned `10.42.0.144`) | *(untested)* |
| Date | 2026-11-08 | *(untested)* |

## Results

| Step | Android | iPhone |
|---|---|---|
| Downloaded `ca.crt` over HTTP | **works** (`http://10.42.0.1:8080/`) | *(untested)* |
| Installed it as a CA certificate | **works** (Settings ➔ Security ➔ Encryption & credentials ➔ Install CA certificate) | *(untested)* |
| Fingerprint on the phone matched the node's | **works** (`D4:95:4B:59:B0:5D:AE...`) | *(untested)* |
| `https://IP:8443/` opens with **no certificate warning** | **works** (green lock; trusted via name-constrained private CA) | *(untested)* |
| Secure context | **works** (`window.isSecureContext = true`) | *(untested)* |
| Service worker registered; files cached | **works** (`scope https://10.42.0.1:8443/; controlling this page`) | *(untested)* |
| IndexedDB | **works** (probe write/read verified; quota 10,240 MiB) | *(untested)* |
| Device key: sign, verify, in-session IndexedDB storage | **works within session** (ECDSA P-256 raw 64-byte signature; non-extractable; stored and read back to sign within page session; cross-restart identity persistence deferred to Day 34) | *(untested)* |
| Node reachable; phone clock within 300 s of the node | **works** (latency 15-30 ms; phone clock is **within a second** of node, rounded to 0 s) | *(untested)* |
| **Page loads with the node unreachable** (OFFLINE LOAD) | **works** (7 verified offline reloads recorded with `controlled: true` by service worker) | *(untested)* |
| Result arrived at the node after reconnecting | **works** (automatically received in `spike-results.jsonl` upon reconnection) | *(untested)* |
| Same again, added to the home screen | *(untested)* | *(untested)* |
| Other browsers tried (Firefox for Android ...) | *(untested; Chrome tested)* | *(untested)* |

## What was hard

| # | What happened | How to make it easier |
|---|---|---|
| 1 | Typing `10.42.0.1:8443` without typing `https://` causes Chrome to send plain HTTP, triggering `Client sent an HTTP request to an HTTPS server`. | Include a direct clickable link on the install page or generate a QR code with the explicit `https://` scheme. |
| 2 | Android does not allow opening `.crt` directly from Downloads; user must navigate into Settings ➔ Security ➔ Encryption & credentials ➔ Install a certificate ➔ CA certificate. | Provide exact step-by-step UI breadcrumb navigation in the field deployment guide. |

## Decision

**Private-CA web app (PWA) approved for Android.**
All critical offline-first requirements verified on real Android hardware:
1. Private name-constrained CA establishes a fully trusted HTTPS secure context with no warnings.
2. Service Worker reliably loads the app from storage with zero network connection (7 loads verified with `controlled: true`).
3. WebCrypto ECDSA P-256 generates non-extractable device keys that can be written to IndexedDB and read back to sign within the session (cross-restart key identity persistence to be tested in Day 34).
4. Clocks synchronized within a second (well within the 300 s HMAC window).
5. Storage persistence returned NOTE (quota 10,240 MiB; subject to standard browser eviction under storage pressure; standalone home-screen mode untested).

*iOS status:* **iOS: untested** (pilot will target Android phones and Linux laptops).  
*Firefox status:* **Firefox: untested** (tested on Chrome Mobile).
