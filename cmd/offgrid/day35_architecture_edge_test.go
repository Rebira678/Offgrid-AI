package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDay35ThreatModelV2Completeness verifies that docs/threat-model.md covers
// all four system boundaries (Node, Phone, Relay, Update path), all 38 STRIDE
// threat entries (N1..N17, P1..P9, R1..R6, U1..U6), valid status annotations,
// explicit non-goals, and consistent status counts.
func TestDay35ThreatModelV2Completeness(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "threat-model.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(raw)

	requiredSections := []string{
		"# Threat model v2",
		"## What is being protected",
		"## The parts",
		"## The attackers",
		"### Node",
		"### Phone",
		"### Relay",
		"### Update path",
		"## What we will not defend against",
		"## Found while writing this",
		"## Summary",
	}
	for _, sec := range requiredSections {
		if !strings.Contains(text, sec) {
			t.Errorf("threat-model.md missing section %q", sec)
		}
	}

	expectIDs := func(prefix string, count int) {
		for i := 1; i <= count; i++ {
			id := fmt.Sprintf("| %s%d |", prefix, i)
			if !strings.Contains(text, id) {
				t.Errorf("threat-model.md missing threat row %q", id)
			}
		}
	}
	expectIDs("N", 17)
	expectIDs("P", 9)
	expectIDs("R", 6)
	expectIDs("U", 6)

	var mitigated, planned, accepted int
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 7 {
			continue
		}
		id := strings.TrimSpace(cols[1])
		if id == "" || id == "#" || strings.HasPrefix(id, "-") {
			continue
		}
		if id[0] != 'N' && id[0] != 'P' && id[0] != 'R' && id[0] != 'U' {
			continue
		}
		status := strings.TrimSpace(cols[5])
		switch {
		case strings.HasPrefix(status, "mitigated"):
			mitigated++
		case strings.HasPrefix(status, "planned"):
			planned++
		case strings.HasPrefix(status, "accepted"):
			accepted++
		default:
			t.Errorf("threat %s has invalid status %q (must start with mitigated, planned, or accepted)", id, status)
		}
	}

	if total := mitigated + planned + accepted; total != 38 {
		t.Errorf("parsed %d threats, want 38 (mitigated=%d planned=%d accepted=%d)", total, mitigated, planned, accepted)
	}
	if !strings.Contains(text, fmt.Sprintf("38 threats: **%d mitigated**, **%d planned**, **%d accepted**", mitigated, planned, accepted)) {
		t.Errorf("summary counts out of sync with parsed table (mitigated=%d planned=%d accepted=%d)", mitigated, planned, accepted)
	}
}

// TestDay35ADRsCompleteAndReversible verifies that ADRs 001 to 006 and their
// index exist and document decision trade-offs and exit criteria cleanly.
func TestDay35ADRsCompleteAndReversible(t *testing.T) {
	adrDir := filepath.Join("..", "..", "docs", "adr")
	indexRaw, err := os.ReadFile(filepath.Join(adrDir, "README.md"))
	if err != nil {
		t.Fatalf("read ADR index: %v", err)
	}
	indexText := string(indexRaw)

	adrs := []string{
		"001-where-the-replica-lives.md",
		"002-device-identity.md",
		"003-what-is-signed.md",
		"004-authorization.md",
		"005-trust-root.md",
		"006-transport.md",
	}
	headings := []string{
		"## Decision",
		"## Rejected",
		"## What it costs",
		"## What would change it",
		"## Evidence so far, and what it changes",
		"## Where the implementation stands",
	}

	for _, name := range adrs {
		if !strings.Contains(indexText, name) {
			t.Errorf("ADR index does not link %s", name)
		}
		raw, err := os.ReadFile(filepath.Join(adrDir, name))
		if err != nil {
			t.Errorf("missing ADR %s: %v", name, err)
			continue
		}
		s := string(raw)
		if !strings.Contains(s, "**Status:**") {
			t.Errorf("%s missing Status line", name)
		}
		for _, h := range headings {
			if !strings.Contains(s, h) {
				t.Errorf("%s missing section %q", name, h)
			}
		}
	}
}

// TestDay35NoStrayTODOCommentsInProductionCode verifies the Day 35 review
// discipline: zero unfinished TODO/FIXME/XXX markers left in production Go code.
func TestDay35NoStrayTODOCommentsInProductionCode(t *testing.T) {
	roots := []string{
		filepath.Join("..", "..", "cmd"),
		filepath.Join("..", "..", "internal"),
	}
	markers := []string{"TODO", "FIXME", "XXX"}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			raw, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			for _, line := range strings.Split(string(raw), "\n") {
				idx := strings.Index(line, "//")
				if idx < 0 {
					continue
				}
				comment := line[idx:]
				for _, m := range markers {
					if strings.Contains(comment, m) {
						t.Errorf("production file %s contains %s marker: %s", p, m, strings.TrimSpace(line))
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestDay35CLIExecCreatesPrivateOwnerOnlyDatabase ensures end-to-end CLI database
// creation via `offgrid exec` leaves the database and WAL/SHM sidecars strictly
// owner-only on POSIX systems.
func TestDay35CLIExecCreatesPrivateOwnerOnlyDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits check")
	}
	t.Setenv("OFFGRID_DB", "")
	db := filepath.Join(t.TempDir(), "cli-private.db")
	code, _, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"secure db check"}`)
	if code != 0 {
		t.Fatalf("exec failed (%d): %s", code, errOut)
	}
	for _, p := range []string{db, db + "-wal", db + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s created with mode %04o; want owner-only", filepath.Base(p), fi.Mode().Perm())
		}
	}
}
