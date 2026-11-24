package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func photoFile(t *testing.T, name string, n int) string {
	t.Helper()
	b := bytes.Repeat([]byte{7}, n)
	copy(b, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0})
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAttachListAndStatsFromTheCommandLine(t *testing.T) {
	db := dbPath(t)
	if code, _, e := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Leaking pump"}`); code != 0 {
		t.Fatal(e)
	}
	f := photoFile(t, "pump.JPG", 4000)
	code, out, e := runCLI(t, "", "store", "-db", db, "attach", "-task", "T1", "-file", f)
	if code != 0 || !strings.Contains(out, "attached pump.JPG (4000 bytes, image/jpeg) to") {
		t.Fatalf("%d\n%s\n%s", code, out, e)
	}
	code, out, _ = runCLI(t, "", "store", "-db", db, "attachments", "-task", "T1")
	if code != 0 || !strings.Contains(out, "image/jpeg") || !strings.Contains(out, "bytes here") || !strings.Contains(out, "pump.JPG") {
		t.Fatalf("%d\n%s", code, out)
	}
	code, out, _ = runCLI(t, "", "store", "-db", db, "blobs")
	if code != 0 || !strings.Contains(out, "1 files") || !strings.Contains(out, "named by no op     0") {
		t.Fatalf("%d\n%s", code, out)
	}
	if code, out, _ := runCLI(t, "", "store", "-db", db, "verify"); code != 0 {
		t.Fatalf("verify: %s", out)
	}
}

func TestAttachRefusesWhatItShould(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"pump"}`)
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no task", []string{"attach", "-file", "x.jpg"}, 2, "-task is required"},
		{"no file", []string{"attach", "-task", "T1"}, 2, "-file is required"},
		{"unknown extension", []string{"attach", "-task", "T1", "-file", photoFile(t, "x.bin", 100)}, 2, "give -type"},
		{"svg", []string{"attach", "-task", "T1", "-file", photoFile(t, "x.svg", 100), "-type", "image/svg+xml"}, 2, "not one this version stores"},
		{"a png that is not", []string{"attach", "-task", "T1", "-file", photoFile(t, "x.png", 100)}, 2, "does not look like image/png"},
		{"missing file", []string{"attach", "-task", "T1", "-file", "/no/such/file.jpg"}, 1, "no such file"},
		{"no such task", []string{"attach", "-task", "T99", "-file", photoFile(t, "y.jpg", 100)}, 1, ""},
	}
	for _, c := range cases {
		code, out, e := runCLI(t, "", append([]string{"store", "-db", db}, c.args...)...)
		if code != c.code || !strings.Contains(out+e, c.want) {
			t.Errorf("%s: exit %d, want %d, containing %q\n%s%s", c.name, code, c.code, c.want, out, e)
		}
	}
}

func TestGCRefusesAShortGraceAndSaysWhatItWouldRemove(t *testing.T) {
	db := dbPath(t)
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"pump"}`)
	runCLI(t, "", "store", "-db", db, "attach", "-task", "T1", "-file", photoFile(t, "a.jpg", 500))
	code, _, e := runCLI(t, "", "store", "-db", db, "gc", "-grace", "1s")
	if code != 2 || !strings.Contains(e, "at least") {
		t.Fatalf("%d %s", code, e)
	}
	// an unreferenced old blob
	blobs := db + ".blobs"
	h := strings.Repeat("ab", 32)
	if err := os.MkdirAll(filepath.Join(blobs, h[:2]), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(blobs, h[:2], h[2:])
	os.WriteFile(p, []byte("orphan"), 0o600)
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(p, old, old)
	code, out, _ := runCLI(t, "", "store", "-db", db, "gc", "-dry-run")
	if code != 0 || !strings.Contains(out, "would remove 1") {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("a dry run removed the file")
	}
	code, out, _ = runCLI(t, "", "store", "-db", db, "gc")
	if code != 0 || !strings.Contains(out, "removed 1") || !strings.Contains(out, "1 named by the log (kept)") {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the orphan was not removed")
	}
}
