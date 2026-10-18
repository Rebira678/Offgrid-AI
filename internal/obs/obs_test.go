package obs

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"offgrid/internal/agent"
	"offgrid/internal/connectivity"
	"offgrid/internal/drain"
	"offgrid/internal/jobqueue"
	"offgrid/internal/router"
)

func jsonLines(t *testing.T, b *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func TestNewLoggerValidatesItsOptions(t *testing.T) {
	if _, err := NewLogger(&bytes.Buffer{}, "text", "loud"); err == nil || !strings.Contains(err.Error(), "-log-level") {
		t.Errorf("%v", err)
	}
	if _, err := NewLogger(&bytes.Buffer{}, "yaml", "info"); err == nil || !strings.Contains(err.Error(), "-log-format") {
		t.Errorf("%v", err)
	}
	for _, lv := range []string{"", "debug", "INFO", "warn", "warning", "error"} {
		if _, err := NewLogger(&bytes.Buffer{}, "", lv); err != nil {
			t.Errorf("level %q: %v", lv, err)
		}
	}
}

func TestNewLoggerFormatAndLevel(t *testing.T) {
	var b bytes.Buffer
	l, _ := NewLogger(&b, "text", "warn")
	l.Info("quiet")
	l.Warn("loud", "k", "v")
	if s := b.String(); strings.Contains(s, "quiet") || !strings.Contains(s, "level=WARN") || !strings.Contains(s, "k=v") {
		t.Errorf("%q", s)
	}
	b.Reset()
	j, _ := NewLogger(&b, "json", "debug")
	j.Debug("hello", "n", 3)
	if m := jsonLines(t, &b); len(m) != 1 || m[0]["msg"] != "hello" || m[0]["level"] != "DEBUG" || m[0]["n"] != float64(3) {
		t.Errorf("%v", m)
	}
}

func TestObserverLogsEachDrainEventAtTheRightLevel(t *testing.T) {
	var b bytes.Buffer
	l, _ := NewLogger(&b, "json", "debug")
	m, _ := newM()
	o := &Observer{Log: l, M: m}
	job := jobqueue.Job{ID: 7, Kind: "interpret", Attempts: 2, MaxAttempts: 5}
	events := []struct {
		e     drain.Event
		level string
		msg   string
	}{
		{drain.Event{Kind: drain.EvClaimed, Job: job, At: t0}, "INFO", "job started"},
		{drain.Event{Kind: drain.EvDone, Job: job, Note: "ok", At: t0}, "INFO", "job done"},
		{drain.Event{Kind: drain.EvRetry, Job: job, Err: errors.New("x"), Delay: time.Second, At: t0}, "WARN", "job failed, will retry"},
		{drain.Event{Kind: drain.EvDead, Job: job, Err: errors.New("x"), At: t0}, "ERROR", "job dead"},
		{drain.Event{Kind: drain.EvReleased, Job: job, Note: "offline", At: t0}, "WARN", "job given back"},
		{drain.Event{Kind: drain.EvLostClaim, Job: job, Note: "n", At: t0}, "WARN", "job lost its claim"},
		{drain.Event{Kind: drain.EvGate, Note: "network lost", At: t0}, "INFO", "network gate"},
		{drain.Event{Kind: drain.EvError, Job: job, Err: errors.New("disk"), At: t0}, "ERROR", "queue problem"},
	}
	for _, c := range events {
		o.Drain(c.e)
	}
	lines := jsonLines(t, &b)
	if len(lines) != len(events) {
		t.Fatalf("%d lines for %d events", len(lines), len(events))
	}
	for i, c := range events {
		if lines[i]["level"] != c.level || lines[i]["msg"] != c.msg {
			t.Errorf("event %d: %v / %v, want %s / %s", i, lines[i]["level"], lines[i]["msg"], c.level, c.msg)
		}
	}
	if lines[0]["job"] != float64(7) || lines[0]["kind"] != "interpret" || lines[0]["attempt"] != float64(2) || lines[0]["max_attempts"] != float64(5) {
		t.Errorf("job fields: %v", lines[0])
	}
	if lines[2]["err"] != "x" || lines[2]["retry_in"] != float64(time.Second) {
		t.Errorf("retry fields: %v", lines[2])
	}
	if lines[1]["result"] != "ok" {
		t.Errorf("result: %v", lines[1])
	}
	// the same events also reached the counters
	if c := m.Snapshot("d", 1, t0, time.Second).Jobs["interpret"]; c.Claimed != 1 || c.Done != 1 || c.Retried != 1 || c.Dead != 1 {
		t.Errorf("%+v", c)
	}
}

func TestObserverLogsLongResultsOnOneShortLine(t *testing.T) {
	var b bytes.Buffer
	l, _ := NewLogger(&b, "json", "info")
	o := &Observer{Log: l}
	o.Drain(drain.Event{Kind: drain.EvDone, Job: jobqueue.Job{ID: 1}, Note: strings.Repeat("word\n", 200), At: t0})
	r, _ := jsonLines(t, &b)[0]["result"].(string)
	if strings.Contains(r, "\n") || len([]rune(r)) > 200 {
		t.Errorf("result not shortened: %d runes", len([]rune(r)))
	}
}

func TestObserverProbesAreDebugOnly(t *testing.T) {
	var b bytes.Buffer
	l, _ := NewLogger(&b, "json", "info")
	o := &Observer{Log: l, M: NewMetrics()}
	o.Probe(connectivity.Result{OK: true}, connectivity.Good, connectivity.Online)
	if b.Len() != 0 {
		t.Errorf("a probe every 2 seconds must not flood the info log: %s", b.String())
	}
	if o.M.Snapshot("x", 1, t0, time.Second).Probes.OK != 1 {
		t.Error("but it must still be counted")
	}
	l2, _ := NewLogger(&b, "json", "debug")
	(&Observer{Log: l2}).Probe(connectivity.Result{OK: false, Err: errors.New("refused"), Target: "t"}, connectivity.Fail, connectivity.Offline)
	m := jsonLines(t, &b)
	if len(m) != 1 || m[0]["level"] != "DEBUG" || m[0]["err"] != "refused" || m[0]["state"] != "OFFLINE" || m[0]["verdict"] == "" {
		t.Errorf("%v", m)
	}
}

func TestObserverLogsTransitionsAndForcedState(t *testing.T) {
	var b bytes.Buffer
	l, _ := NewLogger(&b, "json", "info")
	o := &Observer{Log: l, M: NewMetrics()}
	o.Transition(connectivity.Event{From: connectivity.Online, To: connectivity.Offline, Reason: "3 failed", Last: connectivity.Result{Latency: time.Second}})
	o.Forced(connectivity.Online, "-net online")
	m := jsonLines(t, &b)
	if len(m) != 2 || m[0]["msg"] != "network state changed" || m[0]["from"] != "ONLINE" || m[0]["to"] != "OFFLINE" || m[0]["reason"] != "3 failed" {
		t.Errorf("%v", m)
	}
	if m[1]["msg"] != "network state fixed" || m[1]["state"] != "ONLINE" || m[1]["why"] != "-net online" {
		t.Errorf("%v", m[1])
	}
}

func TestObserverLogsRoutingDecisions(t *testing.T) {
	var b bytes.Buffer
	l, _ := NewLogger(&b, "json", "info")
	m := NewMetrics()
	o := &Observer{Log: l, M: m}
	job := jobqueue.Job{ID: 9}
	o.Route(router.Result{Text: "why is it broken", Route: router.RouteDeferred, Reason: "analysis", Net: connectivity.Offline, Job: &job, Created: true, Total: time.Millisecond})
	o.Route(router.Result{Text: "x", Route: router.RouteFailed, Err: errors.New("disk full")})
	o.Route(router.Result{Text: "list", Route: router.RouteLocal, Outcome: agent.Outcome{Tier: agent.TierRules}})
	lines := jsonLines(t, &b)
	if len(lines) != 3 {
		t.Fatal(len(lines))
	}
	if lines[0]["tier"] != "deferred" || lines[0]["job"] != float64(9) || lines[0]["new"] != true || lines[0]["net"] != "OFFLINE" || lines[0]["reason"] != "analysis" || lines[0]["level"] != "INFO" {
		t.Errorf("%v", lines[0])
	}
	if lines[1]["level"] != "ERROR" || lines[1]["err"] != "disk full" {
		t.Errorf("%v", lines[1])
	}
	if _, has := lines[2]["job"]; has {
		t.Errorf("a locally answered message has no job: %v", lines[2])
	}
	if m.Snapshot("a", 1, t0, time.Second).Tiers[TierDeferred] != 1 {
		t.Error("not counted")
	}
}

func TestNilObserverAndPartsAreHarmless(t *testing.T) {
	var o *Observer
	o.Drain(drain.Event{})
	o.Probe(connectivity.Result{}, connectivity.Good, connectivity.Online)
	o.Transition(connectivity.Event{})
	o.Forced(connectivity.Online, "")
	o.Route(router.Result{})
	// metrics only, logger only
	(&Observer{M: NewMetrics()}).Route(router.Result{Route: router.RouteLocal})
	(&Observer{Log: slogDiscard()}).Drain(drain.Event{Kind: drain.EvDone})
}

func TestOneLine(t *testing.T) {
	if got := oneLine("a \n  b\tc", 10); got != "a b c" {
		t.Errorf("%q", got)
	}
	if got := oneLine("abcdefghij", 5); got != "abcd…" {
		t.Errorf("%q", got)
	}
	if got := oneLine("ééééé", 5); got != "ééééé" {
		t.Errorf("multi-byte text must be cut on characters, not bytes: %q", got)
	}
}
