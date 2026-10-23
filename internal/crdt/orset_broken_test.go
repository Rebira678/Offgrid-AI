package crdt

// The OR-Set laws must catch broken merges, like the LWW ones do (broken_test.go).

import (
	"testing"

	"pgregory.net/rapid"
)

type rapidT = rapid.T

func brokenSetMerges() []struct {
	name     string
	why      string
	merge    setMergeFn
	mustFail []string
} {
	return []struct {
		name     string
		why      string
		merge    setMergeFn
		mustFail []string
	}{
		{
			"tombstones are not merged (only the first argument's are kept)",
			"a remove made elsewhere is silently dropped",
			func(a, b Set) Set {
				out := a.clone()
				for t, e := range b.adds {
					out.putAdd(t, e)
				}
				return out
			},
			[]string{"commutative", "no add and no tombstone is ever lost", "REMOVE HOLDS: a remove is not undone by a replica that never saw it"},
		},
		{
			"tombstones are intersected, not united",
			"a remove only counts if both sides made it, so removed items come back",
			func(a, b Set) Set {
				out := Set{adds: a.clone().adds, removed: map[Tag]struct{}{}}
				for t, e := range b.adds {
					out.putAdd(t, e)
				}
				for t := range a.removed {
					if _, both := b.removed[t]; both {
						out.removed[t] = struct{}{}
					}
				}
				return out
			},
			[]string{"REMOVE HOLDS: a remove is not undone by a replica that never saw it"},
		},
		{
			"adds are intersected, not united",
			"an add only counts if both sides have it, so new items are lost",
			func(a, b Set) Set {
				out := Set{adds: map[Tag]string{}, removed: a.clone().removed}
				for t, e := range a.adds {
					if _, both := b.adds[t]; both {
						out.adds[t] = e
					}
				}
				for t := range b.removed {
					out.removed[t] = struct{}{}
				}
				return out
			},
			[]string{"no add and no tombstone is ever lost", "ADD WINS: a concurrent add survives a remove that did not see it"},
		},
		{
			"the second argument's adds are dropped when it has any tombstone",
			"a remove drags the whole other replica down with it (remove wins)",
			func(a, b Set) Set {
				if len(b.removed) > 0 {
					return a.clone()
				}
				return MergeSet(a, b)
			},
			[]string{"commutative"},
		},
		{
			"merge edits its first argument in place",
			"correct values, but the caller's set changes under them",
			func(a, b Set) Set {
				if a.adds == nil {
					a = Set{adds: map[Tag]string{}, removed: map[Tag]struct{}{}}
				}
				for t, e := range b.adds {
					a.putAdd(t, e)
				}
				for t := range b.removed {
					a.removed[t] = struct{}{}
				}
				return a
			},
			[]string{"does not change its inputs and returns a set of its own"},
		},
	}
}

func TestTheSetLawsCatchEveryDeliberatelyBrokenMerge(t *testing.T) {
	for _, l := range setLaws {
		l := l
		if failed, rep := runProp(t, "Real/"+l.name, func(rt *rapidT) { l.check(rt, MergeSet) }); failed {
			t.Fatalf("the REAL merge failed %q:\n%s", l.name, rep)
		}
	}
	for _, b := range brokenSetMerges() {
		b := b
		t.Run(b.name, func(t *testing.T) {
			caught := map[string]string{}
			for _, l := range setLaws {
				l := l
				if failed, rep := runProp(t, "BrokenSet/"+l.name, func(rt *rapidT) { l.check(rt, b.merge) }); failed {
					caught[l.name] = rep
				}
			}
			for _, name := range b.mustFail {
				rep, ok := caught[name]
				if !ok {
					t.Errorf("the law %q did NOT catch %q", name, b.name)
					continue
				}
				t.Logf("caught by %q after shrinking to:\n%s", name, indent(rep))
			}
			if len(caught) == 0 {
				t.Fatalf("no law caught %q (%s)", b.name, b.why)
			}
		})
	}
}
