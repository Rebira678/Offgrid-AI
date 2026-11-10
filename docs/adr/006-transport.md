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

Not started. Scheduled for Day 40, together with request IDs and replay protection.
