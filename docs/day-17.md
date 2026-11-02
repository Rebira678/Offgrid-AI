# Day 17: Prove the merge is correct

Yesterday's merge works on the examples I thought of. The bugs that sink a sync system are in the examples nobody thought of: equal stamps, a delete against a write, three replicas merging in a different order. Today replaces "I tried some cases" with "I checked thousands I did not choose".

**Done when:** thousands of generated cases pass, then the tie-break is deliberately broken and the tests catch it. Both done.

## Do this first (once)

The tests use the property-testing library `pgregory.net/rapid`. It is a new dependency, so run this in the repo on your machine and commit the change to `go.mod` and `go.sum`:

```bash
go get pgregory.net/rapid@v1.2.0
```

Until you do, `go test ./...` will fail to build the `crdt` and `sqlstore` test files. I pin v1.2.0 because it needs only Go 1.18; the newest release (1.3) wants Go 1.23, which would raise your `go.mod`'s Go version. I could not download the module in my sandbox: I used a local copy of v1.2.0 there, so `go.mod` is not in this zip and the `go.sum` entries have to come from your `go get`.

## What is in the zip

| File | What it is |
|---|---|
| `internal/crdt/props_test.go` | 13 properties over generated maps (below), `TestMain` raising the default to 3000 cases each |
| `internal/crdt/broken_test.go`, `fmt_test.go` | nine merges (seven deliberately broken, two legitimate controls) run through the very same laws |
| `internal/crdt/lwwmap_test.go` | the Day 16 tests, with the broken-clone check made independent |
| `internal/store/sqlstore/fields_test.go`, `main_test.go` | two properties over real op logs, 2000 cases each |
| `scripts/prove-merge.sh` | runs the laws, then the broken-merge demonstration; prints a readable summary |
| `cmd/offgrid/main.go` | version `0.17.0-day17` |
| `ARCHITECTURE.md`, `README.md`, `Makefile` | updated (they overwrite yours: merge by hand if you edited them); `make prove-merge` |
| `docs/day-17.md`, `docs/day-17-properties.{svg,png}` | this file and the diagram |

No production code changed today. The merge is the one from Day 16.

## Concepts

**An example test checks one input; a property test checks all of them.** You state a rule ("merging in either order gives the same result"). The library generates inputs, runs the rule, and when it finds a failure it **shrinks** it: it keeps simplifying the input while the failure persists, until you see the smallest case that breaks. A failing run gives you "two entries, same stamp, different nodes", not a 40-entry map.

**The three laws.** For a merge to make replicas converge without coordination:

- *Commutative*: `merge(a, b) = merge(b, a)`. It does not matter whose copy is "mine".
- *Associative*: `merge(merge(a, b), c) = merge(a, merge(b, c))`. It does not matter how three replicas group their exchanges.
- *Idempotent*: `merge(a, a) = a`. Receiving the same data twice, which a network will do, changes nothing.

Together: any delivery order, any grouping, any duplication, the same final state.

**Generate from tiny domains on purpose.** Values come from 3 letters, keys from 4, stamps from walls 0..3 and counters 0..2, nodes from x, y, z. With big random domains two entries would almost never tie, and the tie-break (the thing most likely to be wrong) would almost never run. Tiny domains make it run in nearly every case.

**Properties beyond the three laws** (all in `props_test.go`):

| Property | What it catches |
|---|---|
| the empty map is the identity | a merge that invents or drops entries |
| keeps every key, never goes backwards (by stamp) | losing data in a merge |
| the larger stamp wins | the LWW rule itself: a merge can obey all three laws and still be the wrong merge |
| inputs are not changed, result is independent | in-place edits that corrupt a replica's own state |
| replicas that exchange everything converge | random writes and partial one-way syncs end identical |
| converged state equals an **oracle** ("the largest entry anyone ever wrote", computed without `Merge`) | a merge that is lawful but wrong |
| writes in any order, with duplicates, give the same map | delivery is unordered and repeating |
| `Put` equals `Merge` with one entry | two code paths agreeing |
| `Compare` is a total order | the root assumption under everything above |
| task keys round-trip | key encoding |

**Two real-data properties** (`sqlstore/fields_test.go`): reading a real op log in any order with duplicates gives the same registers, and **registers built from two partial logs, merged, equal the registers built from the union**. That last one is what makes sync possible: replicas can exchange either ops or registers and reach the same state.

**Break it to prove the tests work.** A test suite that has never failed has never been shown to be able to. `broken_test.go` runs the same laws against merges I broke on purpose. Seven are caught (the sample below is what rapid printed, shrunk):

| Broken merge | Caught by |
|---|---|
| equal stamps keep the first argument (forgot the tie-break) | commutative; replicas converge |
| equal stamps keep the second argument | commutative |
| the counter is ignored | commutative; the larger stamp wins |
| the incoming side always overwrites | commutative; the larger stamp wins |
| the smaller stamp wins | the larger stamp wins; never goes backwards |
| a delete always wins | the larger stamp wins |
| merge edits its first argument in place | inputs not changed |

The first one, shrunk by rapid:

```
a = {title: "a", stamp 0#0, node x}
b = {title: "a", stamp 0#0, node y}
merge(a,b) keeps node x       merge(b,a) keeps node y
```

Two replicas would disagree forever.

**Two controls are accepted.** "On a tie the *lowest* node wins" and "on a tie the larger value wins" are different from the real rule but still a total order, so they are legitimate merges. The laws pass for them. If the laws rejected them the suite would be testing my choice, not the math. That the node-ID rule is arbitrary is exactly what Day 16 said; today shows it is *provably* arbitrary.

Note "the smaller stamp wins" and "a delete always wins" satisfy all three algebraic laws. Only the extra "larger stamp wins" property catches them. Lesson: the laws prove replicas agree, not that they agree on the right answer.

## What was checked

- `gofmt`, `go vet`, `go test -race ./...` pass (with `rapid` available).
- 13 properties x 3000 cases in `crdt`, and 2 x 2000 in `sqlstore`: about 43,000 generated cases per run. `go test -rapid.checks=N` changes the number.
- Mutation testing of the real merge, with the property tests running alone: removing the node tie-break, removing the value tie-break, removing the deleted rule, reversing `Merge`, reversing `Put`, and aliasing the input: all caught (six mutants; a seventh did not apply because the pattern text no longer matched, so it was not run).
- `scripts/prove-merge.sh` run in a scratch copy of the repo with the module set up.

## Honest limits

- **Generated, not exhaustive.** 3000 random cases is strong evidence, not a proof. The domain is small enough that the tests are very likely to hit every combination of tie, delete and counter, but they do not enumerate it.
- The properties test the merge function and the registers built from real logs. They do **not** test sync, because no sync exists yet.
- The real-log properties use six op logs from random workloads (60 ops each). They vary which ops each replica has, not the workloads themselves beyond those six.
- If a property ever fails on your machine, rapid writes a `testdata/rapid/...` file that replays the failure. Commit it as a regression test.

## Try it on your laptop

```bash
go get pgregory.net/rapid@v1.2.0     # once
make prove-merge                      # or: scripts/prove-merge.sh 5000
go test ./internal/crdt -run Laws -v -rapid.checks=20000
# the experiment: in internal/crdt/lwwmap.go, delete the node comparison in Entry.Compare,
# then run: go test ./internal/crdt   (the laws fail, with a shrunk counterexample). Undo it.
```

## Post draft

> "It works on my examples" is how sync bugs get shipped.
>
> Today I tried to break Offgrid's merge instead of trusting it. I wrote the rules it must obey (merge in either order gives the same result; grouping doesn't matter; applying the same data twice changes nothing) and let a property-testing library throw ~43,000 generated cases at them. All pass.
>
> Then the part that matters: I broke the merge on purpose, seven different ways, forgotten tie-break, ignored counter, overwriting, editing in place, and showed the same tests catch every one, each shrunk to a two-entry example. Two "different" merges that are still lawful are accepted, because the tests check the math, not my taste.
>
> The biggest lesson: a merge can obey all three textbook laws and still be wrong ("the OLDER edit wins" does). So there is a fourth property: the later edit wins.
>
> Picture: docs/day-17-properties.png. Code: github link.

Hooks: distributed-systems and testing people; anyone who has been burned by a sync bug.

## Self-check

1. Why draw values, keys and stamps from tiny sets instead of large random ones?
2. Give a merge that is commutative, associative and idempotent but wrong. Which property catches it?
3. What does shrinking do, and why does it matter more than the failure itself?
4. Why are the two controls accepted, and what would it mean if they were not?
5. Why does the "merge of two partial logs equals the registers of the union" property matter for sync?

## Tomorrow: Day 18, OR-Set

Notes are appended by many people. They cannot be a last-writer-wins register (one note would overwrite the others). An observed-remove set keeps every added note, lets a note be removed, and resolves add-versus-remove deterministically. Day 17's property harness will be reused to prove it.
