package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- structured logs and published metrics from the real commands --------

func TestDrainWritesStructuredLogsAndMetrics(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)
	logf := filepath.Join(t.TempDir(), "drain.jsonl")
	mdir := filepath.Join(t.TempDir(), "m")
	code, _, e := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms",
		"-slog", logf, "-slog-format", "json", "-metrics", mdir)
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	b, _ := os.ReadFile(logf)
	var msgs []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		msgs = append(msgs, m["msg"].(string))
		if m["msg"] == "job done" && (m["job"] == nil || m["kind"] == nil || m["result"] == "") {
			t.Errorf("a done line must say which job and what came out: %v", m)
		}
	}
	got := strings.Join(msgs, "|")
	for _, want := range []string{"network state fixed", "job started", "job done"} {
		if !strings.Contains(got, want) {
			t.Errorf("log has no %q: %s", want, got)
		}
	}
	if strings.Count(got, "job done") != 2 {
		t.Errorf("two jobs finished: %s", got)
	}
	if s := readSnaps(t, mdir)["drain"]; s.Proc != "drain" || !s.Stopped {
		t.Errorf("-metrics DIR not honoured: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(queue), "metrics", "drain.json")); err == nil {
		t.Error("with -metrics DIR the default folder must not be used")
	}
}

func TestDrainStructuredLogsDefaultToOffAndOptionsAreChecked(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)
	_, out, e := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms")
	if strings.Contains(out+e, "level=") {
		t.Errorf("structured logs must be opt-in:\n%s%s", out, e)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-slog-level", "loud"}, "-log-level must be"},
		{[]string{"-slog-format", "yaml"}, "-log-format must be"},
		{[]string{"-slog", filepath.Join(t.TempDir(), "no", "dir", "x.log")}, "-slog:"},
	} {
		code, _, e := runDrain(t, append([]string{"-db", db, "-queue", queue, "-once"}, c.args...)...)
		if code != 2 || !strings.Contains(e, c.want) {
			t.Errorf("%v: %d %q", c.args, code, e)
		}
	}
}

func TestDrainSlogDashGoesToStandardError(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)
	_, out, e := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms", "-slog", "-")
	if !strings.Contains(e, "level=INFO msg=\"job done\"") {
		t.Errorf("stderr: %s", e)
	}
	if strings.Contains(out, "level=INFO") {
		t.Errorf("logs must not mix into the readable output:\n%s", out)
	}
}

func TestSlogFileIsAppendedToAcrossRuns(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)
	logf := filepath.Join(t.TempDir(), "offgrid.log")
	runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms", "-slog", logf)
	first, _ := os.ReadFile(logf)
	runDrain(t, "-db", db, "-queue", queue, "-net", "online", "-once", "-poll", "10ms", "-slog", logf)
	second, _ := os.ReadFile(logf)
	if len(first) == 0 || !strings.HasPrefix(string(second), string(first)) || len(second) <= len(first) {
		t.Errorf("a restart must add to the log, not erase what led to it:\n%s\n---\n%s", first, second)
	}
}

func TestMetricsOffWritesNothing(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	defer func() {
		if _, err := os.Stat("off"); err == nil {
			t.Error("-metrics off created a folder called off")
		}
	}()
	os.RemoveAll(filepath.Join(filepath.Dir(queue), "metrics"))
	runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms", "-metrics", "off")
	if _, err := os.Stat(filepath.Join(filepath.Dir(queue), "metrics")); err == nil {
		t.Error("-metrics off still wrote a folder")
	}
}

func TestAgentRouteLogsEachDecisionAndPublishesTiers(t *testing.T) {
	queue := routeQueue(t)
	code, _, e := runAgentRoute(t, "list open tasks\nwhy does the fuel line keep cracking\nshow the queue\n",
		"-seed", "-no-model", "-route", "-net", "offline", "-queue", queue, "-slog", "-", "-slog-format", "json")
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	var routed []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(e), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "routed" {
			routed = append(routed, m)
		}
	}
	if len(routed) != 3 {
		t.Fatalf("%d decisions logged:\n%s", len(routed), e)
	}
	if routed[0]["tier"] != "rules" || routed[1]["tier"] != "deferred" || routed[2]["tier"] != "status" {
		t.Errorf("%v", routed)
	}
	if routed[1]["job"] == nil || routed[1]["net"] != "OFFLINE" || routed[1]["reason"] == "" {
		t.Errorf("a deferred decision must carry the job and why: %v", routed[1])
	}
	s := readSnaps(t, filepath.Join(filepath.Dir(queue), "metrics"))["agent"]
	if s.Tiers["rules"] != 1 || s.Tiers["deferred"] != 1 || s.Tiers["status"] != 1 || !s.Stopped {
		t.Errorf("tiers %v stopped %v", s.Tiers, s.Stopped)
	}
	if s.Queue == nil || s.Queue.Queued != 1 {
		t.Errorf("queue depth in the final snapshot: %+v", s.Queue)
	}
}

func TestAgentWithoutRoutePublishesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OFFGRID_QUEUE", filepath.Join(dir, "queue.db"))
	runAgentRoute(t, "", "-seed", "-no-model", "list open tasks")
	if _, err := os.Stat(filepath.Join(dir, "metrics")); err == nil {
		t.Error("plain agent runs have no queue and no business writing metrics")
	}
}

func TestDrainPublishesRealProbeStateWhenProbing(t *testing.T) {
	useFakeModels(t)
	db, queue := fill(t)
	srv := probeServer(204)
	defer srv.Close()
	ts := srv.URL
	code, _, e := runDrain(t, "-db", db, "-queue", queue, "-net", "auto", "-url", ts, "-probe-interval", "20ms", "-poll", "10ms", "-for", "400ms", "-slog", "-", "-slog-level", "debug")
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	d := readSnaps(t, filepath.Join(filepath.Dir(queue), "metrics"))["drain"]
	if d.Net != "ONLINE" || d.Probes.OK < 3 || d.LastProbe == nil || !d.LastProbe.OK {
		t.Errorf("%+v", d)
	}
	if len(d.Flips) == 0 || d.Flips[0].Reason == "" {
		t.Errorf("a transition must carry its reason: %+v", d.Flips)
	}
	if !strings.Contains(e, "msg=probe") || !strings.Contains(e, "msg=\"network state changed\"") {
		t.Errorf("debug log must show probes and the transition:\n%s", e)
	}
}
