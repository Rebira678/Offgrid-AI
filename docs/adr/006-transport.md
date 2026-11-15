# ADR 006: Transport

**Status:** accepted, 2026-11-09 (from the decisions table of the Days 31 to 60 plan)

## Decision

**Mutual TLS 1.3**, with peers pinned from the membership log. Bootstrap trust comes from a **fingerprint carried in the invite**, so the very first connection can be pinned before any membership exists. Phones use a session token over TLS.

## Rejected

Noise protocol; HMAC behind a proxy.

## What it costs

Certificate plumbing. It does not pass TLS-terminating proxies. Browsers cannot present these certificates, hence the token for phones.

## What would change it

Customers must cross corporate proxies. Then use signed requests over plain TLS.

## Evidence so far, and what it changes

- Two trust arrangements coexist, on purpose: a **browser** trusts the site's private CA (Day 33, name-constrained to private names), a **node** pins another node's device key from the membership log.
- Why both signed ops and TLS: signatures prove who wrote an op; TLS hides it in transit and decides who may download.
- Until this lands, sync is HMAC-signed and **not encrypted** (threat N8), and a stranger can make the node buffer up to 16 MiB per request before authentication (threat N12).

## Where the implementation stands

**Implemented on Day 40.**

- Each device makes a self-signed certificate from its device key (`identity.Certificate`). There is no CA and no chain; a peer checks the **key**, not the certificate: it must be the one the membership log names for an enrolled, unrevoked device (the server by an explicit pin or by the log, a new device's first sync by `-peer-device`).
- TLS 1.3 only, a client certificate required, HTTP/2 with one reused connection, every `http.Server` timeout set.
- A device is re-checked on every request (5 s cache), so a revoked device loses even an open connection.
- Enrollment has its own listener (`sync serve -enroll-addr`): TLS 1.3 with no client certificate, which the new device pins with the admin's device ID and the key fingerprint carried in the invite.
- Request IDs and the time on every request; a repeated ID is refused (409).
- Once a membership exists the shared-key path is off for that node (`sync serve` serves mutual TLS only; `once` and `run` refuse an `http://` peer). A group with no membership still uses the shared key, with its warning.
- Not done: the phone-with-a-token path (phones still use the Day 33 site and the node signs for them), certificate renewal beyond minting a fresh 24 h certificate on demand, and rotating or removing the shared key for a group that has not founded a membership.
