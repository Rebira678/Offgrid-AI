package router

import (
	"strings"
	"testing"
)

// The labelled cases were used to TUNE the gate (the first version, written
// blind, missed 11 of 33 hard cases and had no false defers; see
// docs/day-10.md). So passing them proves little: this is a regression guard.
// The honest estimate is TestGateOnHeldOutCases.
func TestGateOnLabelledCases(t *testing.T) {
	var fd, fl []string
	for _, c := range Cases {
		got := Assess(c.Text).Hard
		if got && !c.Hard {
			fd = append(fd, c.Text)
		}
		if !got && c.Hard {
			fl = append(fl, c.Text)
		}
	}
	t.Logf("cases %d: false defers %d, missed hard %d", len(Cases), len(fd), len(fl))
	if len(fd) != 0 || len(fl) != 0 {
		t.Errorf("the gate no longer agrees with the cases it was tuned on")
	}
	for _, s := range fd {
		t.Logf("  false defer: %q", s)
	}
	for _, s := range fl {
		t.Logf("  missed hard: %q", s)
	}
}

// The scoring rules, one at a time, so each point has a reason that is tested.
func TestGateScoring(t *testing.T) {
	cases := []struct {
		text  string
		score int
		hard  bool
		why   string
	}{
		{"list open tasks", 1, false, "a hint alone never makes a request hard"},
		{"hello", 0, false, "no signal"},
		{"suggest fixes", 2, false, "a weak word alone is not enough: it is also a normal task word"},
		{"check the pump and suggest fixes", 3, true, "a weak word plus a second request joined by 'and'"},
		{"suggest fixes for this week", 3, true, "a weak word plus a time span"},
		{"suggest an order for the open tasks", 3, true, "a weak word plus the whole list"},
		{"recommend a supplier", 3, true, "a strong word is enough alone"},
		{"why", 3, true, "single strong word"},
		{"what should we do", 3, true, "strong phrase"},
		{"summarize this week", 1, false, "a time span alone is the daily_summary tool, not a hard request"},
		{"what is the status of T-3", 0, false, ""},
		{"write a report on the outage", 3, true, "writing a document"},
		{"which site needs attention first", 3, true, "choosing among several"},
		{"I plan to fix the pump at the north site this afternoon for the crew", 3, true, "KNOWN FALSE DEFER: a weak word and a long message look like a hard request"},
		{"week week week", 1, false, "a hint counts once"},
		{"recommend advise assess analyze", 3, true, "strong words do not stack: three points, not twelve"},
	}
	for _, c := range cases {
		got := Assess(c.text)
		if got.Score != c.score || got.Hard != c.hard {
			t.Errorf("%q: score %d hard %v, want %d %v (%s) signals=%v", c.text, got.Score, got.Hard, c.score, c.hard, c.why, got.Signals)
		}
	}
}

func TestGateIgnoresCaseAndPunctuation(t *testing.T) {
	a := Assess("What SHOULD I prioritize, this week?!")
	b := Assess("what should i prioritize this week")
	if a.Score != b.Score || !a.Hard || !b.Hard {
		t.Errorf("%+v vs %+v", a, b)
	}
	// curly apostrophes (phones type them) and possessives
	if !Assess("summarize this week’s incidents and suggest priorities").Hard {
		t.Error("curly apostrophe broke the match")
	}
}

func TestGateWholeWordsOnly(t *testing.T) {
	// "whyte", "planet" and "advised" must not trigger "why", "plan", "advise".
	for _, text := range []string{"call Mr Whyte about the generator", "the planet fitness unit is broken", "Sam advised me yesterday"} {
		if c := Assess(text); c.Score != 0 {
			t.Errorf("%q scored %d (%v)", text, c.Score, c.Signals)
		}
	}
}

func TestGateNeverFails(t *testing.T) {
	for _, s := range []string{"", "   ", "\x00\xff", "??!!", "አማርኛ ጽሑፍ", string(make([]byte, 100000))} {
		_ = Assess(s) // must not panic
	}
	if Assess("").Hard {
		t.Error("empty is not hard")
	}
}

func TestReasonIsReadable(t *testing.T) {
	c := Assess("summarize this week's incidents and suggest priorities")
	if r := c.Reason(); !strings.Contains(r, "suggest") || !strings.Contains(r, "judgement") {
		t.Errorf("reason %q", r)
	}
	if r := Assess("list tasks").Reason(); !strings.Contains(r, "no sign") {
		t.Errorf("reason %q", r)
	}
}
