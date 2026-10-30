package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var tinyPerf = []string{"perf", "-repeats", "1", "-sizes", "30,60", "-jobs", "10", "-workers", "2"}

func TestPerfPrintsTheReportAndWritesTheFiles(t *testing.T) {
	dir := t.TempDir()
	md, js, sv := filepath.Join(dir, "p.md"), filepath.Join(dir, "p.json"), filepath.Join(dir, "p.svg")
	code, out, errOut := runCLI(t, "", append(tinyPerf, "-md", md, "-json", js, "-svg", sv)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"**Hardware:**", "**Store**", "**Sync**", "**Queue**", "new replica, HTTP on localhost"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	b, err := os.ReadFile(md)
	if err != nil || string(b) != out {
		t.Fatalf("the -md file differs from what was printed (%v)", err)
	}
	if svg, err := os.ReadFile(sv); err != nil || !strings.HasPrefix(string(svg), "<svg") || !strings.Contains(string(svg), "A new replica catches up") {
		t.Fatalf("svg: %v", err)
	}
	raw, _ := os.ReadFile(js)
	var rep struct {
		Meta struct{ Seed int64 } `json:"meta"`
		Sync []struct{ Ops int }  `json:"sync"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil || rep.Meta.Seed != 1 || len(rep.Sync) != 6 {
		t.Fatalf("json: %v %+v", err, rep)
	}
}

func TestPerfRewritesOnlyTheBlockBetweenTheMarkers(t *testing.T) {
	f := filepath.Join(t.TempDir(), "README.md")
	before := "# Title\n\nkeep this\n\n" + perfStart + "\nOLD\n" + perfEnd + "\n\nand this\n"
	os.WriteFile(f, []byte(before), 0o644)
	code, _, errOut := runCLI(t, "", append(tinyPerf, "-readme", f)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	got, _ := os.ReadFile(f)
	s := string(got)
	if !strings.HasPrefix(s, "# Title\n\nkeep this\n\n"+perfStart+"\n**Hardware:**") || !strings.HasSuffix(s, perfEnd+"\n\nand this\n") || strings.Contains(s, "OLD") {
		t.Fatalf("README after:\n%s", s)
	}
	if _, err := os.Stat(f + ".tmp"); err == nil {
		t.Error("the temporary file was left behind")
	}
}

func TestPerfLeavesAReadmeWithoutMarkersAloneAndFails(t *testing.T) {
	f := filepath.Join(t.TempDir(), "README.md")
	os.WriteFile(f, []byte("no markers here\n"), 0o644)
	code, _, errOut := runCLI(t, "", append(tinyPerf, "-readme", f)...)
	if code != 1 || !strings.Contains(errOut, perfStart) {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if b, _ := os.ReadFile(f); string(b) != "no markers here\n" {
		t.Fatalf("README changed: %q", b)
	}
	if code, _, _ := runCLI(t, "", append(tinyPerf, "-readme", filepath.Join(t.TempDir(), "missing.md"))...); code != 1 {
		t.Fatalf("missing README: exit %d", code)
	}
}

func TestPerfRefusesBadArguments(t *testing.T) {
	for _, args := range [][]string{{"-repeats", "0"}, {"-sizes", "10,x"}, {"-sizes", ""}, {"-jobs", "0"}, {"-workers", "-1"}, {"extra"}, {"-nope"}} {
		if code, _, _ := runCLI(t, "", append([]string{"perf"}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestIntList(t *testing.T) {
	got, err := intList(" 100, 1000 ,10000 ")
	if err != nil || len(got) != 3 || got[2] != 10000 {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "a", "0", "-3", "1.5", "1,,x"} {
		if _, err := intList(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
