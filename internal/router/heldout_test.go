package router

import "testing"

func TestGateOnHeldOutCases(t *testing.T) {
	var fd, fl []string
	hard := 0
	for _, c := range HeldOut {
		if c.Hard {
			hard++
		}
		got := Assess(c.Text).Hard
		if got && !c.Hard {
			fd = append(fd, c.Text)
		}
		if !got && c.Hard {
			fl = append(fl, c.Text)
		}
	}
	// Guards against getting WORSE than when this was measured (1 false defer,
	// 10 missed of 20 hard). These are not quality targets.
	if len(fd) > 1 || len(fl) > 10 {
		t.Errorf("held-out result got worse: %d false defers (was 1), %d missed (was 10)", len(fd), len(fl))
	}
	t.Logf("held-out %d cases (%d hard): false defers %d, missed hard %d", len(HeldOut), hard, len(fd), len(fl))
	for _, s := range fd {
		t.Logf("  false defer: %q", s)
	}
	for _, s := range fl {
		t.Logf("  missed hard: %q", s)
	}
}
