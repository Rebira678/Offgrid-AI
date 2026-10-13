package connectivity

import (
	"math/rand"
	"strings"
	"testing"
)

// run feeds a string of verdicts ("g" good, "s" slow, "f" fail) into a fresh
// machine and returns the state after each probe.
func run(t *testing.T, th Thresholds, seq string) []State {
	t.Helper()
	m, err := NewMachine(th)
	if err != nil {
		t.Fatal(err)
	}
	var out []State
	for _, c := range seq {
		m.Observe(verdictOf(t, c))
		out = append(out, m.State())
	}
	return out
}

func verdictOf(t *testing.T, c rune) Verdict {
	switch c {
	case 'g':
		return Good
	case 's':
		return Slow
	case 'f':
		return Fail
	}
	t.Fatalf("bad verdict letter %q", c)
	return Fail
}

func final(t *testing.T, th Thresholds, seq string) State {
	s := run(t, th, seq)
	return s[len(s)-1]
}

var th = Thresholds{DownAfter: 3, DegradeAfter: 2, UpAfter: 3}

func TestFirstProbeDecidesAtOnce(t *testing.T) {
	for seq, want := range map[string]State{"g": Online, "s": Degraded, "f": Offline} {
		if got := final(t, th, seq); got != want {
			t.Errorf("first probe %q: got %v, want %v", seq, got, want)
		}
	}
}

func TestTransitions(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want State
	}{
		{"stays online on good", "gggggg", Online},
		{"one failure does not matter", "gggfggg", Online},
		{"two failures degrade but do not take it offline", "ggffg", Degraded},
		{"a failure then a success changes nothing", "ggfg", Online},
		{"two slow in a row degrade", "ggss", Degraded},
		{"one slow does not", "ggsgg", Online},
		{"slow then fail degrade", "ggsf", Degraded},
		{"three fails go offline", "gggfff", Offline},
		{"degraded goes offline on three fails", "ggssfff", Offline},
		{"degraded recovers after three good", "ggssggg", Online},
		{"degraded needs three good, two is not enough", "ggssgg", Degraded},
		{"a slow probe resets the good streak", "ggssggsgg", Degraded},
		{"offline needs three answers", "fffgg", Offline},
		{"offline plus three fast is online", "fffggg", Online},
		{"offline plus mixed answers is degraded", "fffgsg", Degraded},
		{"a failure resets the recovery", "fffggfgg", Offline},
		{"flapping never changes state", "gfgfgfgfgfgf", Online},
		{"flapping while offline stays offline", "ffgfgfgfgf", Offline},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := final(t, th, c.seq); got != c.want {
				t.Errorf("%s: got %v, want %v (states: %v)", c.seq, got, c.want, run(t, th, c.seq))
			}
		})
	}
}

func TestTransitionReportsFromToAndReason(t *testing.T) {
	m, _ := NewMachine(th)
	m.Observe(Good)
	m.Observe(Fail)
	m.Observe(Fail) // degraded here
	tr := m.Observe(Fail)
	if tr == nil || tr.From != Degraded || tr.To != Offline {
		t.Fatalf("transition = %+v, want DEGRADED -> OFFLINE", tr)
	}
	if !strings.Contains(tr.Reason, "3") {
		t.Errorf("reason %q should say how many probes", tr.Reason)
	}
	if m.Observe(Fail) != nil {
		t.Error("a further failure while already offline must not report a transition")
	}
}

// Streaks describe the last probes, not the state, so evidence keeps counting
// across a transition: offline takes three failures in total, not three more
// after the state already degraded.
func TestStreaksSurviveTransitions(t *testing.T) {
	states := run(t, th, "gggfff")
	want := []State{Online, Online, Online, Online, Degraded, Offline}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}
	// The slow probes that caused DEGRADED are not good probes, so they never
	// count towards recovery.
	if got := final(t, th, "ggss"+"gg"); got != Degraded {
		t.Errorf("got %v, want DEGRADED", got)
	}
	// The failures that caused OFFLINE are not answers.
	if got := final(t, th, "gggfff"+"gg"); got != Offline {
		t.Errorf("got %v, want OFFLINE", got)
	}
}

func TestThresholdsOfOne(t *testing.T) {
	one := Thresholds{DownAfter: 1, DegradeAfter: 1, UpAfter: 1}
	if got := final(t, one, "gf"); got != Offline {
		t.Errorf("down after 1: %v", got)
	}
	if got := final(t, one, "gfg"); got != Online {
		t.Errorf("up after 1: %v", got)
	}
}

func TestInvalidThresholdsRefused(t *testing.T) {
	for _, bad := range []Thresholds{{}, {DownAfter: 0, DegradeAfter: 1, UpAfter: 1}, {DownAfter: 1, DegradeAfter: -1, UpAfter: 1}} {
		if _, err := NewMachine(bad); err == nil {
			t.Errorf("NewMachine(%+v) should fail", bad)
		}
	}
}

// The properties that make this a hysteresis machine, checked on random input
// rather than hand-picked sequences. For every transition that happens we look
// back at the probes that caused it.
func TestHysteresisProperties(t *testing.T) {
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		cfg := Thresholds{DownAfter: 1 + rng.Intn(4), DegradeAfter: 1 + rng.Intn(4), UpAfter: 1 + rng.Intn(4)}
		m, _ := NewMachine(cfg)
		var hist []Verdict
		prev := Unknown
		for i := 0; i < 400; i++ {
			v := Verdict(rng.Intn(3))
			if rng.Intn(3) > 0 && len(hist) > 0 {
				v = hist[len(hist)-1] // runs, so streaks actually happen
			}
			hist = append(hist, v)
			tr := m.Observe(v)

			if (tr == nil) != (m.State() == prev) {
				t.Fatalf("seed %d step %d: transition=%v but state %v -> %v", seed, i, tr, prev, m.State())
			}
			if tr != nil {
				if tr.From != prev || tr.To == tr.From {
					t.Fatalf("seed %d step %d: bad transition %+v (prev %v)", seed, i, tr, prev)
				}
				if prev != Unknown {
					switch tr.To {
					case Offline:
						if !lastN(hist, cfg.DownAfter, func(x Verdict) bool { return x == Fail }) {
							t.Fatalf("seed %d step %d: went OFFLINE without %d failures in a row: %v", seed, i, cfg.DownAfter, tail(hist, 6))
						}
					case Online:
						if !lastN(hist, cfg.UpAfter, func(x Verdict) bool { return x == Good }) {
							t.Fatalf("seed %d step %d: went ONLINE without %d good in a row: %v", seed, i, cfg.UpAfter, tail(hist, 6))
						}
					case Degraded:
						if prev == Online && !lastN(hist, cfg.DegradeAfter, func(x Verdict) bool { return x != Good }) {
							t.Fatalf("seed %d step %d: degraded without %d bad in a row", seed, i, cfg.DegradeAfter)
						}
						if prev == Offline && !lastN(hist, cfg.UpAfter, func(x Verdict) bool { return x != Fail }) {
							t.Fatalf("seed %d step %d: left OFFLINE without %d answers in a row", seed, i, cfg.UpAfter)
						}
					}
				}
			}
			prev = m.State()
		}
	}
}

func lastN(h []Verdict, n int, ok func(Verdict) bool) bool {
	if len(h) < n {
		return false
	}
	for _, v := range h[len(h)-n:] {
		if !ok(v) {
			return false
		}
	}
	return true
}

func tail(h []Verdict, n int) []Verdict {
	if len(h) < n {
		return h
	}
	return h[len(h)-n:]
}

func TestStrings(t *testing.T) {
	if Online.String() != "ONLINE" || Degraded.String() != "DEGRADED" || Offline.String() != "OFFLINE" || Unknown.String() != "UNKNOWN" {
		t.Error("state names changed")
	}
	if Good.String() != "good" || Slow.String() != "slow" || Fail.String() != "fail" {
		t.Error("verdict names changed")
	}
}
