package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorOnAHealthyDatabase(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	db := filepath.Join(t.TempDir(), "t.db")
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"a"}`); code != 0 {
		t.Fatalf("exec: %d %s", code, e)
	}
	code, out, errOut := runCLI(t, "", "doctor", "-db", db, "-listen", "", "-deep")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"PASS  version", "PASS  database", "hash chain intact", "changes nothing", "0 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorFailsOnAMissingDatabaseAndCreatesNothing(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	dir := t.TempDir()
	db := filepath.Join(dir, "missing.db")
	code, out, _ := runCLI(t, "", "doctor", "-db", db, "-listen", "")
	if code != 1 || !strings.Contains(out, "FAIL  database") || !strings.Contains(out, "no such database") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("doctor created %d files", len(ents))
	}
}

func TestDoctorJSONAndUsage(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	code, out, errOut := runCLI(t, "", "doctor", "-listen", "", "-json")
	if code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	var cs []struct{ Name, Status, Detail string }
	if err := json.Unmarshal([]byte(out), &cs); err != nil || len(cs) == 0 {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	for _, c := range cs {
		if c.Status != "PASS" && c.Status != "WARN" && c.Status != "FAIL" {
			t.Errorf("status %q", c.Status)
		}
	}
	for _, args := range [][]string{{"doctor", "extra"}, {"doctor", "-min-free-mb", "0"}, {"doctor", "-nope"}} {
		if code, _, _ := runCLI(t, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if _, out, _ := runCLI(t, "", "help"); !strings.Contains(out, "offgrid doctor") {
		t.Error("usage does not list doctor")
	}
}

func TestDoctorFlagsABusyPort(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	code, out, _ := runCLI(t, "", "doctor", "-listen", ln.Addr().String())
	if code != 0 || !strings.Contains(out, "WARN  port") {
		t.Errorf("a busy port should warn, not fail: exit %d\n%s", code, out)
	}
}
