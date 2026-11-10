# ADR 001: Where the replica lives

**Status:** accepted, 2026-11-09 (from the decisions table of the Days 31 to 60 plan)

## Decision

A site node holds the log. It is either **full** (a laptop or mini-PC with a local model) or **light** (a Pi: sync, web and rules, the model optional on another node). Phones are thin clients on the LAN.

## Rejected

Go on every phone; the browser as a full replica.

## What it costs

A phone out of range can only queue edits. In return there is far less to build and test.

## What would change it

Pilot crews often work outside the LAN. Then build the browser or phone replica: the merge packages already compile to WebAssembly.

## Evidence so far, and what it changes

- Day 31: `make wasm` guards that `internal/crdt` and `internal/hlc` stay standard-library only and build for WebAssembly, so the escape hatch stays open.
- Day 33: on one real Android phone (Chrome), the page loaded with the node unreachable, seven times, served by the service worker, and the result reached the node after reconnecting. A browser that clears its storage under pressure would lose a queued edit (threat P6).
- Day 32: the hardware decision for the *light* node (a Pi) is still undecided; no Pi has run Offgrid yet.

## Where the implementation stands

The node exists. The full/light split is not yet written into the product. iOS is untested.
