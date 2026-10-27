package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

func TestAnswersEdgeUnexpectedArguments(t *testing.T) {
	db, _ := fill(t)
	var out, errBuf bytes.Buffer
	code := run([]string{"answers", "-db", db, "unexpected_arg"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("expected exit code 2 for unexpected arguments, got: %d", code)
	}
	if !strings.Contains(errBuf.String(), "unexpected argument") {
		t.Fatalf("expected error message to mention unexpected argument, got: %q", errBuf.String())
	}
}

func TestAnswersEdgeCorruptDatabaseFile(t *testing.T) {
	corruptPath := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(corruptPath, []byte("NOT A SQLITE DATABASE"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := run([]string{"answers", "-db", corruptPath}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("expected exit code 1 for corrupt database, got: %d", code)
	}
	if !strings.Contains(errBuf.String(), "error:") {
		t.Fatalf("expected error output, got: %q", errBuf.String())
	}
}

func TestAnswersEdgeMultipleAnswersHumanAndJSONOutput(t *testing.T) {
	dbPath, _ := fill(t)
	ctx := context.Background()

	// Open the task store directly to record multiple Tier 2 answers
	st, err := sqlstore.Open(ctx, dbPath, sqlstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.RecordAnswer(ctx, "k-1", "How is battery voltage?", "Battery voltage is 48.2V.", "qwen2.5:7b", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordAnswer(ctx, "k-2", "Is generator oil clean?", "Generator oil level optimal, filter replaced.", "deepseek-r1:70b", "cloud"); err != nil {
		t.Fatal(err)
	}

	// Test Human Output Format
	var humanOut, humanErr bytes.Buffer
	code := run([]string{"answers", "-db", dbPath}, &humanOut, &humanErr)
	if code != 0 {
		t.Fatalf("human answers failed: code %d, err: %s", code, humanErr.String())
	}
	humanStr := humanOut.String()
	if !strings.Contains(humanStr, "Q: How is battery voltage?") ||
		!strings.Contains(humanStr, "Battery voltage is 48.2V.") ||
		!strings.Contains(humanStr, "(local)") {
		t.Fatalf("missing local answer in human output:\n%s", humanStr)
	}
	if !strings.Contains(humanStr, "Q: Is generator oil clean?") ||
		!strings.Contains(humanStr, "Generator oil level optimal") ||
		!strings.Contains(humanStr, "(cloud)") {
		t.Fatalf("missing cloud answer in human output:\n%s", humanStr)
	}

	// Test JSON Output Format
	list := answersJSON(t, dbPath)
	if len(list) != 2 {
		t.Fatalf("expected 2 answers in JSON, got %d", len(list))
	}
	if list[0].Key != "k-1" || list[0].Source != "local" {
		t.Fatalf("unexpected answer 0: %+v", list[0])
	}
	if list[1].Key != "k-2" || list[1].Source != "cloud" {
		t.Fatalf("unexpected answer 1: %+v", list[1])
	}
}

func TestDrainEdgeKeyFileReadAndTrim(t *testing.T) {
	useFakeModels(t)
	gate(t, true)
	calls, urls := fakeCloud(t, "KEYFILE ANSWER", nil)
	db, queue := fill(t)

	// Write key with surrounding whitespace & newlines
	keyFile := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(keyFile, []byte("  \t\nsk-clean-secret-key\n\n  "), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errBuf := runDrain(t, "-db", db, "-queue", queue, "-net", "online", "-once", "-poll", "10ms",
		"-cloud-url", "https://api.cloud.test", "-cloud-model", "big-cloud", "-cloud-key-file", keyFile)
	if code != 0 {
		t.Fatalf("drain failed with code %d: %s", code, errBuf)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 cloud call, got %d", calls.Load())
	}
	if len(*urls) == 0 {
		t.Fatal("cloud client was never constructed")
	}
	lastURL := (*urls)[len(*urls)-1]
	expectedTarget := "https://api.cloud.test|sk-clean-secret-key"
	if lastURL != expectedTarget {
		t.Fatalf("cloud client received key %q, want %q", lastURL, expectedTarget)
	}

	a := answersJSON(t, db)
	if len(a) != 1 || a[0].Source != "cloud" || a[0].Text != "KEYFILE ANSWER" {
		t.Fatalf("unexpected answer in DB: %+v", a)
	}
}

func TestDrainEdgeZeroIdleWaitExitsImmediatelyWhenBusy(t *testing.T) {
	useFakeModels(t)
	gate(t, false) // Busy machine
	db, queue := fill(t)

	start := time.Now()
	code, out, errBuf := runDrain(t, "-db", db, "-queue", queue, "-net", "offline", "-once", "-poll", "10ms", "-idle-wait", "0")
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("drain exited with %d: %s", code, errBuf)
	}
	// With -idle-wait 0, it should exit in under 500ms without waiting for a quiet machine
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("drain took too long (%v), should have exited immediately with -idle-wait 0", elapsed)
	}
	if kinds := queueKinds(t, queue); !strings.Contains(kinds, "deep_think") {
		t.Fatalf("deep_think job should have remained queued:\n%s", kinds)
	}
	if !strings.Contains(out, "1 still queued") && !strings.Contains(out, "queue now: 1 queued") {
		t.Fatalf("expected output to mention job still queued:\n%s", out)
	}
}

func TestAnswersCmdJSONEmptyArray(t *testing.T) {
	db, _ := fill(t)
	var out, errBuf bytes.Buffer
	code := run([]string{"answers", "-db", db, "-json"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("answers -json failed: %d, %s", code, errBuf.String())
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("expected exactly '[]', got: %q", out.String())
	}
}

func TestAnswersCmdHelpUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"answers", "-h"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("expected code 2 on -h, got %d", code)
	}
	usage := errBuf.String()
	if !strings.Contains(usage, "-db") || !strings.Contains(usage, "-json") {
		t.Fatalf("usage missing flags:\n%s", usage)
	}
}

func TestDrainCmdConcurrencyAndTimeoutValidation(t *testing.T) {
	db, queue := fill(t)

	badFlags := [][]string{
		{"-concurrency", "0"},
		{"-concurrency", "65"},
		{"-idle-max-busy", "-0.1"},
		{"-idle-max-busy", "1.1"},
		{"-no-model"},
		{"unexpected-positional-arg"},
	}

	for _, flags := range badFlags {
		t.Run(strings.Join(flags, "_"), func(t *testing.T) {
			args := append([]string{"drain", "-db", db, "-queue", queue, "-once"}, flags...)
			var out, errBuf bytes.Buffer
			code := run(args, &out, &errBuf)
			if code != 2 {
				t.Fatalf("expected exit code 2 for flags %v, got %d: %s", flags, code, errBuf.String())
			}
			if !strings.Contains(errBuf.String(), "error:") {
				t.Fatalf("expected error output for flags %v, got: %q", flags, errBuf.String())
			}
		})
	}
}
