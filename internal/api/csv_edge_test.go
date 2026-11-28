package api

import (
	"context"
	"strings"
	"testing"

	"offgrid/internal/task"
)

func TestCSVAdvancedEdgeCases(t *testing.T) {
	r := newRig(t, nil)

	// Test formula injection characters coverage: '=', '+', '-', '@', '\t', '\r'
	testChars := []string{
		"=SUM(A1:A10)",
		"+100.50",
		"-42.00",
		"@user_mention",
		"\ttab_prefix",
		"\rcarriage_prefix",
		"'=already_quoted_formula",
		"''=double_quoted_formula",
	}

	for _, tc := range testChars {
		esc := escapeCell(tc)
		if esc != "" && strings.ContainsRune("=+-@\t\r", rune(esc[0])) {
			t.Fatalf("escapeCell(%q) = %q still starts with formula char", tc, esc)
		}
		unesc := unescapeCell(esc)
		if unesc != tc {
			t.Fatalf("escape/unescape mismatch for %q: got %q", tc, unesc)
		}
	}

	// CSV Import with headers having extra whitespace, different casing, and UTF-8 BOM
	bomCSV := "\xef\xbb\xbf  TiTlE  ,  PRIORITY  ,  sTaTuS  ,  SITE  ,  NOTES  \r\n" +
		"Pump maintenance,high,open,Substation A,\"[\"\"inspected belt\"\", \"\"ordered spare\"\"]\"\r\n" +
		"=Formula title,low,in_progress,Site 2,plain note\r\n"

	rep, err := ImportCSV(context.Background(), r.st, strings.NewReader(bomCSV), false)
	if err != nil {
		t.Fatalf("ImportCSV failed: %v", err)
	}
	if rep.Created != 2 || len(rep.Failed) != 0 || len(rep.Skipped) != 0 {
		t.Fatalf("unexpected import report: %+v", rep)
	}

	// Verify imported tasks in store
	ts, err := r.st.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 {
		t.Fatalf("expected 2 tasks in store, got %d", len(ts))
	}

	// Test import rejection when title column is missing
	noTitleCSV := "name,site,status\nTask A,Site 1,open\n"
	_, err = ImportCSV(context.Background(), r.st, strings.NewReader(noTitleCSV), false)
	if err == nil || !strings.Contains(err.Error(), `"title" column`) {
		t.Fatalf("expected error about missing title column, got %v", err)
	}

	// Test dry-run idempotency
	dryRunCSV := "title,status\nDry Task 1,open\nDry Task 2,open\n"
	repDry, err := ImportCSV(context.Background(), r.st, strings.NewReader(dryRunCSV), true)
	if err != nil || !repDry.DryRun || repDry.Created != 2 {
		t.Fatalf("expected dry run success, got %+v, err: %v", repDry, err)
	}

	// Verify store was not modified by dry run
	tsAfterDry, _ := r.st.List(context.Background(), task.Filter{})
	if len(tsAfterDry) != len(ts) {
		t.Fatalf("store modified during dry run: before=%d, after=%d", len(ts), len(tsAfterDry))
	}
}
