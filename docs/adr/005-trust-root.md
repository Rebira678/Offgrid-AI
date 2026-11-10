# ADR 005: Trust root

**Status:** accepted, 2026-11-09 (from the decisions table of the Days 31 to 60 plan)

## Decision

**A founder key**, with more admins added by admins. **Require a second admin before the pilot.**

## Rejected

One key, no recovery.

## What it costs

A stolen admin key is catastrophic, and the recovery is a re-founding of the group. The docs must say so.

## What would change it

Customers want company single sign-on. That is a later layer, not this plan.

## Evidence so far, and what it changes

- This is the least reversible choice of the six: whoever founds a group holds its root. It is also the one a new user is least able to judge, so the setup guide has to explain it in plain words.
- `SECURITY.md` and the threat model list a stolen admin key under "what we will not defend against".

## Where the implementation stands

Not started. The founder is created with enrollment (Day 38).
