package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteThenCheckPasses(t *testing.T) {
	dir := t.TempDir()
	db, acks := filepath.Join(dir, "d.db"), filepath.Join(dir, "acks.txt")
	var out, errb bytes.Buffer
	if c := run([]string{"write", "-for", "300ms", "-writers", "4", db}, &out, &errb); c != 0 {
		t.Fatalf("write exit %d: %s", c, errb.String())
	}
	if !strings.Contains(out.String(), "ack ") {
		t.Fatal("no acks printed")
	}
	os.WriteFile(acks, out.Bytes(), 0o600)
	var res bytes.Buffer
	if c := run([]string{"check", db, acks}, &res, &errb); c != 0 || !strings.HasPrefix(res.String(), "PASS") {
		t.Fatalf("check: %d %s", c, res.String())
	}
}

func TestCheckCatchesALostWrite(t *testing.T) {
	dir := t.TempDir()
	db, acks := filepath.Join(dir, "d.db"), filepath.Join(dir, "acks.txt")
	var out, errb bytes.Buffer
	run([]string{"write", "-for", "100ms", "-writers", "2", db}, &out, &errb)
	forged := out.String() + "ack pp1-g0-999999\n" // an ack for a write that never happened
	os.WriteFile(acks, []byte(forged), 0o600)
	var res bytes.Buffer
	if c := run([]string{"check", db, acks}, &res, &errb); c != 1 || !strings.Contains(res.String(), "FAIL") {
		t.Fatalf("a lost write went unnoticed: %d %s", c, res.String())
	}
	_ = time.Second
}

func TestUsage(t *testing.T) {
	var o, e bytes.Buffer
	if run(nil, &o, &e) != 2 || run([]string{"nope", "x"}, &o, &e) != 2 {
		t.Fatal("bad usage must exit 2")
	}
}
