package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The power-pull kit (scripts/real-device) is only worth carrying to a Pi if the
// checker can tell a lost write from a good run. These tests run the real scripts
// against the real binary, on a database made on the spot.

func kitScript(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", "real-device", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func runKit(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", args...)
	cmd.Env = append(os.Environ(), "OFFGRID_BIN="+bin)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestPowerPullCheckerSeparatesAGoodRunFromALostWrite(t *testing.T) {
	bin := buildBinary(t)
	check := kitScript(t, "powerpull-check.sh")
	dir := t.TempDir()
	db := filepath.Join(dir, "t.db")
	for _, title := range []string{"pp-deadbeef-1-1", "pp-deadbeef-1-2", "pp-deadbeef-2-1", "pp-deadbeef-2-2"} {
		if _, e, err := runBin(t, bin, nil, "exec", "-strict", "-db", db, `{"intent":"create_task","title":"`+title+`"}`); err != nil {
			t.Fatalf("seed: %v %s", err, e)
		}
	}
	acks := func(lines ...string) string {
		p := filepath.Join(dir, "acks.log")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name  string
		lines []string
		code  int
		want  string
	}{
		{"all acknowledged writes present", []string{"RUN deadbeef", "ACK pp-deadbeef-1-1", "ACK pp-deadbeef-1-2", "ACK pp-deadbeef-2-1"}, 0, "RESULT: PASS  acked=3 present=4 lost=0"},
		{"written but never acknowledged is allowed", []string{"ACK pp-deadbeef-1-1"}, 0, "written but not acknowledged:  3"},
		{"an acknowledged write is missing", []string{"ACK pp-deadbeef-1-1", "ACK pp-deadbeef-9-9"}, 1, "lost: pp-deadbeef-9-9"},
		{"a line cut off mid-write is not an ACK", []string{"ACK pp-deadbeef-1-1", "ACK pp-deadbeef-1-"}, 0, "acked=1 "},
		{"errors are not acknowledgements", []string{"ACK pp-deadbeef-1-1", "ERR pp-deadbeef-7-7"}, 0, "lost=0"},
		{"an empty log tested nothing", []string{"RUN deadbeef"}, 1, "nothing was tested"},
	}
	for _, c := range cases {
		out, code := runKit(t, bin, check, db, acks(c.lines...))
		if code != c.code || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want %d containing %q\n%s", c.name, code, c.code, c.want, out)
		}
	}
}

func TestPowerPullCheckerFailsOnADamagedDatabase(t *testing.T) {
	bin := buildBinary(t)
	check := kitScript(t, "powerpull-check.sh")
	dir := t.TempDir()
	db := filepath.Join(dir, "t.db")
	for i := 0; i < 40; i++ { // enough ops to fill several pages
		title := fmt.Sprintf("filler-%d-%s", i, strings.Repeat("x", 80))
		runBin(t, bin, nil, "exec", "-db", db, `{"intent":"create_task","title":"`+title+`"}`)
	}
	raw, err := os.ReadFile(db)
	if err != nil || len(raw) < 3*4096 {
		t.Skipf("database too small to damage (%d bytes, %v)", len(raw), err)
	}
	for i := 4096; i < 3*4096; i++ {
		raw[i] = 0xFF
	}
	if err := os.WriteFile(db, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	acks := filepath.Join(dir, "acks.log")
	os.WriteFile(acks, []byte("ACK pp-deadbeef-1-1\n"), 0o644)
	out, code := runKit(t, bin, check, db, acks)
	// The failure must come from the doctor step, not merely from the unrelated missing ACK.
	if code == 0 || !strings.Contains(out, "FAIL: doctor reported a failure") {
		t.Errorf("a damaged database passed the check (exit %d)\n%s", code, out)
	}
}

// The writer prints ACK only for commands that succeeded, and every ACK is really in the database.
func TestPowerPullWriterOnlyAcknowledgesWhatIsStored(t *testing.T) {
	bin := buildBinary(t)
	writer := kitScript(t, "powerpull-writer.sh")
	db := filepath.Join(t.TempDir(), "t.db")

	cmd := exec.Command("bash", writer, db, "2")
	cmd.Env = append(os.Environ(), "OFFGRID_BIN="+bin)
	var out syncBuilder
	cmd.Stdout = &out
	cmd.SysProcAttr = killGroup()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	killProcessGroup(cmd) // the writer never ends by itself; kill it and everything it started
	_ = cmd.Wait()

	var acked []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "ACK ") {
			acked = append(acked, strings.TrimPrefix(l, "ACK "))
		}
	}
	if len(acked) < 3 {
		t.Fatalf("only %d ACKs in 2 s:\n%s", len(acked), out.String())
	}
	ops, _, _ := runBin(t, bin, nil, "store", "-db", db, "ops", "-n", "100000")
	for _, title := range acked {
		if !strings.Contains(ops, `"title":"`+title+`"`) {
			t.Errorf("acknowledged %s but it is not in the database", title)
		}
	}
	if !strings.Contains(out.String(), "RUN ") {
		t.Error("the writer did not print its run id")
	}
}

// syncBuilder is a strings.Builder that the process and the test may touch at once.
type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A failing doctor must fail the run even when every ACK is present: a database
// can hold all the rows and still be damaged. A stand-in binary isolates that rule.
func TestPowerPullCheckerFailsWhenOnlyTheDoctorFails(t *testing.T) {
	check := kitScript(t, "powerpull-check.sh")
	dir := t.TempDir()
	stub := filepath.Join(dir, "offgrid")
	script := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  doctor) echo 'FAIL  database  stubbed damage'; exit 1;;\n" +
		"  store) echo '{\"title\":\"pp-deadbeef-1-1\"}';;\n" +
		"esac\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	acks := filepath.Join(dir, "acks.log")
	if err := os.WriteFile(acks, []byte("ACK pp-deadbeef-1-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runKit(t, stub, check, filepath.Join(dir, "x.db"), acks)
	if code != 1 || !strings.Contains(out, "lost=0") || !strings.Contains(out, "RESULT: FAIL") {
		t.Errorf("exit %d, want 1 with lost=0 and RESULT: FAIL\n%s", code, out)
	}
}
