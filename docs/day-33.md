# Day 33: can a phone run Offgrid with no internet?

**Done when:** a written result for each OS you could test (works, works with caveats, or fails), an OS you could not test written down as **untested**, and a recorded decision: private-CA web app, online-only page, or native wrapper after the pilot.

**What is done and what is not.** The tools and the page are built and rehearsed in a desktop browser. **No phone has been tested.** That test is yours, and nothing here counts as a phone result until `docs/phone-spike-report.md` has a date, a phone model and an Android version in it.

```bash
offgrid ca init  -dir ~/offgrid-ca                       # once per site; ca.key is SECRET
offgrid ca issue -dir ~/offgrid-ca -out ~/node -name 192.168.1.20 -name offgrid.local
offgrid ca spike -cert ~/node/node.crt -key ~/node/node.key -ca ~/offgrid-ca/ca.crt
```

## Why a certificate authority at all

A phone browser only runs the things Offgrid needs (service workers for offline pages, WebCrypto for device keys, a stored key that cannot be copied out) on a **secure context**: HTTPS, or `localhost`. Plain HTTP to `http://192.168.1.20` gets none of them. A public certificate (Let's Encrypt) needs the internet to be issued and renewed, which a site with no internet cannot do. So the site owner makes a small CA once, installs its certificate on each phone once, and the node serves certificates signed by it. The cost is that one-time install on every phone, and it is harder on iPhone.

## What the tools do

| Command | What it does |
|---|---|
| `ca init -dir D` | Makes `ca.crt` (public) and `ca.key` (SECRET, mode 0600) in `D`. Refuses to overwrite an existing CA: every phone that trusts the old one would stop working. Prints the fingerprint. |
| `ca issue -dir D -out N -name X ...` | Signs `node.crt` and `node.key` for the names and IP addresses the phone will type. Needs `ca.key`. Valid 365 days by default, at most 825 (iPhones refuse longer). Checks its own work: the new certificate must verify against the CA for every name, or nothing is written. |
| `ca show FILE` | Names, dates, days left, what a CA may issue for, fingerprint. An expired certificate says so. A CA with no name constraints says that loudly. |
| `ca spike ...` | The phone check. An HTTPS page that tests the phone, plus a plain-HTTP page whose only job is to hand over `ca.crt`. Stops by itself after an hour. |

Everything uses `crypto/x509`, `crypto/ecdsa` (P-256) and `crypto/rand`; no dependencies.

### Why this is safe enough to ask a person to do

A CA installed on a phone is trusted for **every website**. If its key leaked, an attacker could impersonate a bank to that phone. So:

- **The CA is name-constrained** (a critical x509 extension). It may only vouch for private addresses (10/8, 172.16/12, 192.168/16, link-local, loopback, their IPv6 equivalents) and the names `.local`, `.lan`, `.offgrid`. A certificate it signs for `example.com` is refused by the browser, and `ca issue` refuses to create one. Tested both ways, including a certificate forged directly with the CA key. `-only` narrows it further (a single subnet), `-permit` widens it.
- **It cannot sign another CA** (path length 0), and a node certificate is not a CA.
- **The CA key never needs to be on the node.** The node needs `node.crt`, `node.key` and `ca.crt`. `ca spike` warns if `ca.key` is sitting next to `ca.crt`. Put it on a USB stick.
- **Files are created exclusively and private** (`ca.key` and `node.key` mode 0600, the folder 0700); loading a key that other users can read is refused, as is a key that does not belong to its certificate.
- **Certificates are backdated one day.** A phone or Pi that lost its clock is often hours behind, and "not valid yet" looks exactly like a broken CA.
- **The plain-HTTP page serves only `ca.crt` and an install page.** The CA certificate is public. Anything else on that port is a 404 (tested, including `/ca.key`).

## The phone check page

One page, about 200 lines of JavaScript, no libraries, no inline script (the Content-Security-Policy would block it). It records each check as PASS, FAIL or NOTE:

| Check | Why it matters |
|---|---|
| Secure context, address uses https | Without it nothing below works: the CA is not installed or not trusted |
| Service worker registered; page files cached | Offline pages |
| IndexedDB write and read | The offline queue and the local copy |
| Storage kept when the phone is low on space (NOTE, never FAIL) | The browser may clear storage under pressure; installing to the home screen usually helps |
| Device key: ECDSA P-256 sign and verify | The same curve as the node's device keys (Day 34) |
| Private key cannot be read out | A key created non-extractable stays that way |
| Device key survives storage in IndexedDB | The key object is stored, read back and signs again: a key that dies on restart is useless |
| Device ID can be derived from the public key | Day 34's device ID is a hash of the public key |
| Node reachable; phone clock matches the node | Sync is refused when clocks differ by more than 5 minutes (`clock_skew`), so a phone with a wrong clock is a sync failure |
| **Page loads with the node unreachable** | The real test: after one visit, the page loads from the phone's own storage with the node gone. It is recorded on the phone, and sent to the node when the phone is back online |

The page posts its result to the node by itself (`/report`, one JSON line each in `spike-results.jsonl`), so you do not type anything from the phone. If it cannot reach the node there is a "Copy as text" button.

## What I verified, and with what

**Unit and integration tests.** `internal/pki` (14 tests; 11 of 12 mutations caught, the survivor is a duplicate guard: `InitCA` and the exclusive file create both refuse to overwrite), `internal/spike` (a real TLS server: a client with the CA connects, one without it is refused, one that asks for a different name is refused; headers; the report endpoint's limits; what plain HTTP will and will not serve), `cmd/offgrid/cacmd_test.go` (including old TLS refused and an expired node certificate refused). Mutation-tested: every one of the 13 mutations I ran on the server and command code is caught after I added two tests for the first two survivors (TLS minimum version, expired certificate).

**A real browser, headless Chromium** (`scripts/real-device/phone-check-selftest.py`): all 12 checks pass; reload; stop the node and cut the browser's network; reload: **OFFLINE LOAD, served by the service worker**; restart the node, go online, reload: the page sends its result by itself and the node's record includes the offline load. This run **found two bugs in my page** that unit tests could not: I had `crypto.subtle.verify` arguments in the wrong order (every device-key check would have failed on every phone), and a browser refusing persistent storage was shown as FAIL instead of a NOTE. Both fixed.

**What this does not prove.** Chromium on Linux is not an Android phone. In the rehearsal the certificate is trusted with `--ignore-certificate-errors` and `127.0.0.1` is always a secure context, so the one thing the phone test is *for*, whether a phone accepts the installed CA, was **not** exercised. The service worker, offline cache, IndexedDB and WebCrypto behaviour of Android Chrome and of iOS Safari can differ from desktop Chromium.

## The phone test (yours)

**Before you start**
1. On the laptop, on the same Wi-Fi as the phone: `ip -4 addr` and note the address (say `192.168.1.20`). A router can change it; if it does, issue a new node certificate.
2. Make the CA and the node certificate with that address as a `-name` (commands at the top). Use the **IP address**, the surest name; `offgrid.local` works only if the phone resolves `.local`, which Android's Chrome does not always do. The phone must open the same name that is in the certificate, or it shows a name-mismatch warning. That is the commonest way this test fails and it looks like a broken CA; `ca spike` warns if the certificate is not for any of this machine's addresses.
3. `offgrid ca spike ...` prints two addresses: `http://IP:8080/` (install the CA) and `https://IP:8443/` (the check).
4. Make sure the phone's clock is right and it has a screen lock (Android will not install a CA without one).

**Android** (record the model and the Android version)
1. On the phone, open `http://192.168.1.20:8080/` and download the certificate.
2. Install it as a CA: **Settings, Security (or Security and privacy), More security settings, Encryption and credentials, Install a certificate, CA certificate**. Android warns that "the network may be monitored"; that is the warning every installed CA gets, and the CA is limited by name as above. (On Android 11 and later, tapping the downloaded file does not install it; use the Settings path.) Names of the menus vary by manufacturer.
3. To check you have the right certificate: Settings, Security, Encryption and credentials, **Trusted credentials, User**, tap it, and compare the SHA-256 fingerprint with the one printed on the node.
4. In **Chrome**, open `https://192.168.1.20:8443/`. No certificate warning is the first result. Wait for the checks, then the page sends its result.
5. **Offline test:** Ctrl-C the node on the laptop (and, for the cleanest test, turn off the phone's Wi-Fi), then reload or reopen the page from history. It must say **OFFLINE LOAD**. Restart the node, reconnect, reload: the result arrives, with the offline load in it.
6. Repeat once with the page **added to the home screen** (Chrome menu, Add to Home screen or Install) and note whether it behaves differently.
7. Other browsers: Firefox for Android keeps its own certificate store and by default does not trust a CA installed in Android's settings; it needs a setting changed. Record it as untested unless you try.

**iPhone** (only if you can borrow one; otherwise write **iOS: untested** in the report, and the docs say so). Expected steps, not yet tried by me: download the certificate in Safari, install the profile (Settings, General, VPN and Device Management), then **enable full trust** for it (Settings, General, About, Certificate Trust Settings). Known from experience, to be confirmed by the test: Safari clears a website's stored data after about seven days without a visit unless the page is added to the home screen, which matters for an offline-first app.

**Send me:** `spike-results.jsonl` (one line per load) and the report filled in. Each line carries the browser, the platform, every check's result and detail, and the offline-load evidence.

## Decision table

| What you saw | Decision |
|---|---|
| Android works, every check PASS, offline load works | **Private-CA web app (PWA)**, as planned. Pairing and the CA install go in the setup guide |
| Works, but only after the home-screen install, or persistence is refused | PWA with "add to home screen" as a required setup step |
| Android fails on the CA install or the offline load | **Online-only page** (no offline reads on the phone) for the pilot, and a native wrapper after it |
| iOS fails or is untested | Say so in the docs: pilot on Android and laptops only until iOS is tested |

## Your part

1. Merge two lines into your `main.go` (I did not send `main.go`, to keep your version string): in the `switch` add `case "ca": return cmdCA(args[1:], stdout, stderr)` next to `case "doctor":`, and add the line `  offgrid ca init|issue|show|spike  a small private certificate authority for HTTPS on the site network (see offgrid ca help)` to the usage text.
2. `make test && make wasm-test && make lint`.
3. Optional rehearsal in your own Chromium: `pip install playwright && playwright install chromium`, then `make phone-selftest`.
4. Run the phone test above on the Android phone, fill in `docs/phone-spike-report.md`, and send me `spike-results.jsonl`.
