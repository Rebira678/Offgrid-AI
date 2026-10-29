package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"offgrid/internal/hlc"
)

func TestLWWDemoShowsBothEditsKeptTheLaterWinningAndTheNodeIDTieBreak(t *testing.T) {
	code, out, errOut := runCLI(t, "", "lww", "demo")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"SIMULATION",
		"Lidia after merging:   priority=high   status=in_progress",
		"Beka after merging:    priority=high   status=in_progress",
		"Lidia after merging:   priority=high   status=open",
		"Beka after merging:    priority=high   status=open",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "THEY DIFFER") {
		t.Errorf("replicas diverged:\n%s", out)
	}
	if n := strings.Count(out, "both ended with the same data"); n != 3 {
		t.Errorf("%d agreements, want 3:\n%s", n, out)
	}
	if strings.Contains(out, "priority=low") {
		t.Errorf("a losing value was shown after a merge:\n%s", out)
	}
}

func TestLWWCommandErrors(t *testing.T) {
	for _, args := range [][]string{{"lww"}, {"lww", "nope"}, {"lww", "demo", "extra"}} {
		if code, _, _ := runCLI(t, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, _, e := runCLI(t, "", "lww", "help"); code != 0 || !strings.Contains(e, "SIMULATION") {
		t.Errorf("help: %d %q", code, e)
	}
}

func TestStoreFieldsListsOneRegisterPerFieldWithItsWinningStampAndNode(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, "-now", "2026-10-08T09:00", `{"intent":"create_task","title":"Leaking pump","site":"4","priority":"low"}`)
	runCLI(t, "", "exec", "-db", db, "-now", "2026-10-08T09:30", `{"intent":"update_task","task_ref":"pump","priority":"high"}`)
	code, out, errOut := runCLI(t, "", "store", "-db", db, "fields")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		got[m["key"].(string)] = m
	}
	if len(got) != 4 {
		t.Fatalf("want 4 registers (title, site, priority, status), got %d:\n%s", len(got), out)
	}
	// IDs carry the replica ("T1.n-1a2b3c"), which the CLI picks at random.
	var pri, title map[string]any
	for k, m := range got {
		switch {
		case strings.HasPrefix(k, "task/T1.") && strings.HasSuffix(k, "/priority"):
			pri = m
		case strings.HasPrefix(k, "task/T1.") && strings.HasSuffix(k, "/title"):
			title = m
		}
	}
	if pri == nil || title == nil {
		t.Fatalf("no T1 priority/title register in:\n%s", out)
	}
	if pri["value"] != "high" || pri["node"] == "" || title["value"] != "Leaking pump" {
		t.Errorf("registers: %v %v", pri, title)
	}
	// -now is read in the machine's time zone, so compare the two stamps, not the clock face
	ps, err1 := hlc.Parse(pri["hlc"].(string))
	ts, err2 := hlc.Parse(title["hlc"].(string))
	if err1 != nil || err2 != nil || ps.Time().Sub(ts.Time()) != 30*time.Minute {
		t.Errorf("priority was written 30 minutes after the title: %v %v (%v %v)", pri["hlc"], title["hlc"], err1, err2)
	}
}
