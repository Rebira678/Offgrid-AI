package crdt

// "Do the property tests actually test anything?" The only way to know is to
// break the merge on purpose and watch them fail. These are the breakages,
// most of them the ones people really write: forgetting the tie-break, letting
// the argument order decide, ignoring part of the timestamp.
//
// The same laws that pass for the real Merge (props_test.go) run against each
// broken merge here. Two of the variants are CONTROLS: they change the rule
// but keep it a total order, so they are legitimate merges, and the laws must
// still pass. A property suite that rejected those would be testing the
// choice, not the math.

import (
	"flag"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"
)

// recorder is a rapid.TB that records failures instead of failing the test.
type recorder struct {
	mu     sync.Mutex
	name   string
	failed bool
	out    strings.Builder
}

func (r *recorder) Helper()      {}
func (r *recorder) Name() string { return r.name }
func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out.WriteString(strings.TrimRight(sprintf(format, args...), "\n") + "\n")
}
func (r *recorder) Logf(f string, a ...any)   { r.logf(f, a...) }
func (r *recorder) Log(a ...any)              { r.logf("%s", sprint(a...)) }
func (r *recorder) Skipf(f string, a ...any)  { r.logf(f, a...); runtime.Goexit() }
func (r *recorder) Skip(a ...any)             { r.logf("%s", sprint(a...)); runtime.Goexit() }
func (r *recorder) SkipNow()                  { runtime.Goexit() }
func (r *recorder) Errorf(f string, a ...any) { r.logf(f, a...); r.Fail() }
func (r *recorder) Error(a ...any)            { r.logf("%s", sprint(a...)); r.Fail() }
func (r *recorder) Fatalf(f string, a ...any) { r.logf(f, a...); r.FailNow() }
func (r *recorder) Fatal(a ...any)            { r.logf("%s", sprint(a...)); r.FailNow() }
func (r *recorder) FailNow()                  { r.Fail(); runtime.Goexit() }
func (r *recorder) Fail()                     { r.mu.Lock(); r.failed = true; r.mu.Unlock() }
func (r *recorder) Failed() bool              { r.mu.Lock(); defer r.mu.Unlock(); return r.failed }

// runLaw runs one law against a merge and reports whether rapid found a
// counterexample, plus rapid's report (the shrunk counterexample). It runs in
// a scratch directory so rapid's fail files do not land in the source tree.
func runLaw(t *testing.T, l law, merge mergeFn) (failed bool, report string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	rec := &recorder{name: "Broken/" + l.name}
	done := make(chan struct{})
	go func() {
		defer close(done)
		rapid.Check(rec, func(rt *rapid.T) { l.check(rt, merge) })
	}()
	<-done
	return rec.Failed(), rec.out.String()
}

// pickBy builds a merge that keeps, per key, whichever entry `wins` prefers.
// wins(cur, incoming) reports whether incoming should replace cur.
func pickBy(wins func(cur, in Entry) bool) mergeFn {
	return func(a, b Map) Map {
		out := independentMap(a)
		for k, e := range b {
			if cur, ok := out[k]; !ok || wins(cur, e) {
				out[k] = e
			}
		}
		return out
	}
}

var brokenMerges = []struct {
	name     string
	why      string
	merge    mergeFn
	mustFail []string // laws that have to catch it; empty for a control
}{
	{
		"no tie-break: equal stamps keep the first argument",
		"the classic: compare stamps, forget what to do when they are equal",
		pickBy(func(cur, in Entry) bool { return in.Stamp.Compare(cur.Stamp) > 0 }),
		[]string{"commutative", "replicas that exchange everything converge"},
	},
	{
		"no tie-break: equal stamps keep the second argument",
		"the same mistake with >= instead of >",
		pickBy(func(cur, in Entry) bool { return in.Stamp.Compare(cur.Stamp) >= 0 }),
		[]string{"commutative"},
	},
	{
		"the counter is ignored",
		"stamps compared by wall time and node only",
		pickBy(func(cur, in Entry) bool {
			if in.Stamp.Wall != cur.Stamp.Wall {
				return in.Stamp.Wall > cur.Stamp.Wall
			}
			return in.Node > cur.Node
		}),
		[]string{"commutative", "the larger stamp wins (the LWW rule itself)"},
	},
	{
		"the incoming side always overwrites",
		"merge(a, b) = b's entries on top of a's, with no comparison at all",
		func(a, b Map) Map {
			out := independentMap(a)
			for k, e := range b {
				out[k] = e
			}
			return out
		},
		[]string{"commutative", "the larger stamp wins (the LWW rule itself)"},
	},
	{
		"the smaller stamp wins",
		"every algebraic law still holds (min is as lawful as max); only the LWW rule is violated",
		pickBy(func(cur, in Entry) bool { return in.Compare(cur) < 0 }),
		[]string{"the larger stamp wins (the LWW rule itself)", "keeps every key and never goes backwards"},
	},
	{
		"a delete always wins",
		"lawful (an order on deleted-first) but a newer write can never bring a key back",
		pickBy(func(cur, in Entry) bool {
			if cur.Deleted != in.Deleted {
				return in.Deleted
			}
			return in.Compare(cur) > 0
		}),
		[]string{"the larger stamp wins (the LWW rule itself)"},
	},
	{
		"merge edits its first argument in place",
		"correct values, but the caller's map changes under them",
		func(a, b Map) Map {
			if a == nil {
				a = Map{}
			}
			for k, e := range b {
				if cur, ok := a[k]; !ok || e.Compare(cur) > 0 {
					a[k] = e
				}
			}
			return a
		},
		[]string{"does not change its inputs and returns a map of its own"},
	},
	// ---- controls: different, but still a total order. The laws must pass.
	{
		"CONTROL: on a tie the LOWEST node ID wins",
		"a different arbitrary choice; every replica still agrees",
		pickBy(func(cur, in Entry) bool {
			if c := in.Stamp.Compare(cur.Stamp); c != 0 {
				return c > 0
			}
			if in.Node != cur.Node {
				return in.Node < cur.Node
			}
			return in.Compare(cur) > 0
		}),
		nil,
	},
	{
		"CONTROL: on a tie the larger VALUE wins, then the node",
		"also arbitrary, also total",
		pickBy(func(cur, in Entry) bool {
			if c := in.Stamp.Compare(cur.Stamp); c != 0 {
				return c > 0
			}
			if in.Value != cur.Value {
				return in.Value > cur.Value
			}
			return in.Compare(cur) > 0
		}),
		nil,
	},
}

func TestTheLawsCatchEveryDeliberatelyBrokenMergeAndAcceptTheControls(t *testing.T) {
	// the breakages show up within a few hundred cases; 1000 keeps this test quick
	if f := flag.Lookup("rapid.checks"); f != nil {
		old := f.Value.String()
		if !testing.Short() && old == "3000" {
			_ = flag.Set("rapid.checks", "1000")
			defer flag.Set("rapid.checks", old)
		}
	}
	// first, the real merge passes everything (the same laws, the same harness)
	for _, l := range laws {
		if failed, rep := runLaw(t, l, Merge); failed {
			t.Fatalf("the REAL merge failed %q:\n%s", l.name, rep)
		}
	}
	for _, b := range brokenMerges {
		b := b
		t.Run(b.name, func(t *testing.T) {
			caught := map[string]string{}
			for _, l := range laws {
				if failed, rep := runLaw(t, l, b.merge); failed {
					caught[l.name] = rep
				}
			}
			if len(b.mustFail) == 0 {
				if len(caught) != 0 {
					for n, r := range caught {
						t.Errorf("a legitimate merge was rejected by %q:\n%s", n, r)
					}
				}
				t.Logf("accepted by all %d laws (%s)", len(laws), b.why)
				return
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

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
