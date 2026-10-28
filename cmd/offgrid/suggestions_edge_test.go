package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"offgrid/internal/task"
)

// TestSuggestionsCLIEdgeMalformedAndBoundaryIDs tests boundary cases and malformed
// IDs passed to accept and dismiss subcommands.
func TestSuggestionsCLIEdgeMalformedAndBoundaryIDs(t *testing.T) {
	db := aiSeededDB(t)

	malformedIDs := []string{
		".12",
		"laptop.",
		"laptop.abc",
		"laptop.1.2",
		"laptop.-5",
		"laptop.999999999999999999999999999999999999999999",
		"laptop/12",
		"laptop:12",
		";DROP TABLE suggestions;--",
		" ",
		"\t",
	}

	for _, id := range malformedIDs {
		t.Run("accept_"+id, func(t *testing.T) {
			code, _, stderr := runCmd(t, "suggestions", "-db", db, "accept", id)
			if code != 1 {
				t.Fatalf("expected code 1 for malformed ID %q, got %d (stderr: %s)", id, code, stderr)
			}
			if !strings.Contains(stderr, "error:") {
				t.Fatalf("expected stderr to contain 'error:', got: %s", stderr)
			}
		})

		t.Run("dismiss_"+id, func(t *testing.T) {
			code, _, stderr := runCmd(t, "suggestions", "-db", db, "dismiss", id)
			if code != 1 {
				t.Fatalf("expected code 1 for malformed ID %q, got %d (stderr: %s)", id, code, stderr)
			}
			if !strings.Contains(stderr, "error:") {
				t.Fatalf("expected stderr to contain 'error:', got: %s", stderr)
			}
		})
	}
}

// TestSuggestionsCLIEdgeJSONOutputVariations tests JSON serialization of suggestions
// across various states: empty, pending, stale, accepted, and dismissed.
func TestSuggestionsCLIEdgeJSONOutputVariations(t *testing.T) {
	db := aiSeededDB(t)

	// 1. Empty list returns empty JSON array []
	code, out, stderr := runCmd(t, "suggestions", "-db", db, "-json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	var emptyList []task.Suggestion
	if err := json.Unmarshal([]byte(out), &emptyList); err != nil {
		t.Fatalf("failed to parse empty JSON: %v, out: %s", err, out)
	}
	if emptyList == nil || len(emptyList) != 0 {
		t.Fatalf("expected non-nil empty slice, got %+v", emptyList)
	}

	// 2. Propose a change via AI (T1 has priority high by default, so propose low)
	code, _, stderr = runCmd(t, "exec", "-as-ai", "-by", "qwen2.5:7b", "-db", db, `{"intent":"update_task","task_ref":"T1","priority":"low","status":"in_progress"}`)
	if code != 0 {
		t.Fatalf("exec -as-ai failed: %d %s", code, stderr)
	}

	// 3. Inspect JSON output for pending suggestion
	code, out, stderr = runCmd(t, "suggestions", "-db", db, "-json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	var pendingList []task.Suggestion
	if err := json.Unmarshal([]byte(out), &pendingList); err != nil {
		t.Fatalf("failed to parse pending JSON: %v, out: %s", err, out)
	}
	if len(pendingList) != 1 {
		t.Fatalf("expected 1 suggestion, got %d", len(pendingList))
	}
	sug1 := pendingList[0]
	if sug1.State != task.SuggestionPending {
		t.Fatalf("expected state pending, got %s", sug1.State)
	}
	if sug1.Stale {
		t.Fatalf("expected not stale yet")
	}
	if sug1.By != "qwen2.5:7b" {
		t.Fatalf("expected by qwen2.5:7b, got %s", sug1.By)
	}
	if sug1.Priority == nil || *sug1.Priority != task.PriorityLow {
		t.Fatalf("unexpected draft priority: %v", sug1.Priority)
	}
	if sug1.Status == nil || *sug1.Status != task.StatusInProgress {
		t.Fatalf("unexpected draft status: %v", sug1.Status)
	}

	// 4. Human modifies T1, making the suggestion stale (human sets priority to medium)
	code, _, stderr = runCmd(t, "exec", "-db", db, `{"intent":"update_task","task_ref":"T1","priority":"medium"}`)
	if code != 0 {
		t.Fatalf("human update failed: %d %s", code, stderr)
	}

	// 5. Inspect JSON output for stale suggestion
	code, out, stderr = runCmd(t, "suggestions", "-db", db, "-json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	var staleList []task.Suggestion
	if err := json.Unmarshal([]byte(out), &staleList); err != nil {
		t.Fatalf("failed to parse stale JSON: %v", err)
	}
	if len(staleList) != 1 || !staleList[0].Stale {
		t.Fatalf("expected 1 stale suggestion, got %+v", staleList)
	}

	// 6. Dismiss the stale suggestion
	code, _, stderr = runCmd(t, "suggestions", "-db", db, "dismiss", sug1.ID)
	if code != 0 {
		t.Fatalf("dismiss failed: %d %s", code, stderr)
	}

	// 7. Verify dismissed suggestion appears in -all -json
	code, out, stderr = runCmd(t, "suggestions", "-db", db, "-all", "-json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	var allList []task.Suggestion
	if err := json.Unmarshal([]byte(out), &allList); err != nil {
		t.Fatalf("failed to parse all JSON: %v", err)
	}
	if len(allList) != 1 || allList[0].State != task.SuggestionDismissed {
		t.Fatalf("expected 1 dismissed suggestion in -all, got %+v", allList)
	}
}

// TestSuggestionsCLIEdgeConcurrentResolutions verifies that concurrent CLI accept/dismiss
// commands safely serialize and only one succeeds.
func TestSuggestionsCLIEdgeConcurrentResolutions(t *testing.T) {
	db := aiSeededDB(t)

	// Create a suggestion
	runCmd(t, "exec", "-as-ai", "-by", "lidia-agent", "-db", db, `{"intent":"update_task","task_ref":"T1","priority":"low"}`)
	_, out, _ := runCmd(t, "suggestions", "-db", db)
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("no suggestion listed: %s", out)
	}
	sugID := fields[0]

	const workers = 6
	var wg sync.WaitGroup
	wg.Add(workers)

	type result struct {
		code   int
		stdout string
		stderr string
	}
	results := make([]result, workers)

	for i := 0; i < workers; i++ {
		workerIdx := i
		go func() {
			defer wg.Done()
			code, stdout, stderr := runCmd(t, "suggestions", "-db", db, "accept", sugID)
			results[workerIdx] = result{code: code, stdout: stdout, stderr: stderr}
		}()
	}
	wg.Wait()

	successCount := 0
	failureCount := 0
	for _, res := range results {
		if res.code == 0 {
			successCount++
			if !strings.Contains(res.stdout, "accepted "+sugID) {
				t.Errorf("expected accepted message, got: %s", res.stdout)
			}
		} else {
			failureCount++
			if !strings.Contains(res.stderr, "already accepted or dismissed") {
				t.Errorf("expected already accepted error, got: %s", res.stderr)
			}
		}
	}

	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful accept, got %d (failures: %d)", successCount, failureCount)
	}
	if failureCount != workers-1 {
		t.Fatalf("expected %d failures, got %d", workers-1, failureCount)
	}
}

// TestSuggestionsCLIEdgeDismissThenAcceptRefused verifies that once a suggestion is dismissed,
// an accept attempt is cleanly refused.
func TestSuggestionsCLIEdgeDismissThenAcceptRefused(t *testing.T) {
	db := aiSeededDB(t)

	runCmd(t, "exec", "-as-ai", "-by", "beka-agent", "-db", db, `{"intent":"update_task","task_ref":"T1","priority":"low"}`)
	_, out, _ := runCmd(t, "suggestions", "-db", db)
	fields := strings.Fields(out)
	if len(fields) == 0 || !strings.Contains(out, "on T1") {
		t.Fatalf("unexpected suggestions list: %s", out)
	}
	sugID := fields[0]

	code, outDismiss, errDismiss := runCmd(t, "suggestions", "-db", db, "dismiss", sugID)
	if code != 0 {
		t.Fatalf("dismiss failed: %d %s %s", code, outDismiss, errDismiss)
	}

	code, outAccept, errAccept := runCmd(t, "suggestions", "-db", db, "accept", sugID)
	if code != 1 {
		t.Fatalf("expected accept after dismiss to exit with 1, got %d (stdout: %s, stderr: %s)", code, outAccept, errAccept)
	}
	if !strings.Contains(errAccept, "already accepted or dismissed") {
		t.Fatalf("expected already accepted or dismissed message, got: %s", errAccept)
	}
}

// TestSuggestionsCLIEdgeNoteProposalAndAcceptance verifies notes proposed by AI can be accepted
// and formatted properly.
func TestSuggestionsCLIEdgeNoteProposalAndAcceptance(t *testing.T) {
	db := aiSeededDB(t)

	noteContent := "Inspection complete: battery health at 94% with nominal solar yield"
	cmdStr := fmt.Sprintf(`{"intent":"add_note","task_ref":"T1","note":%q}`, noteContent)

	code, out, stderr := runCmd(t, "exec", "-as-ai", "-by", "telemetry-ai", "-db", db, cmdStr)
	if code != 0 {
		t.Fatalf("exec -as-ai note failed: %d %s", code, stderr)
	}
	if !strings.Contains(out, "suggested, NOT applied") {
		t.Fatalf("unexpected exec output: %s", out)
	}

	code, listOut, _ := runCmd(t, "suggestions", "-db", db)
	if code != 0 || !strings.Contains(listOut, "add the note") {
		t.Fatalf("suggestion list did not show note proposal: %s", listOut)
	}
	sugID := strings.Fields(listOut)[0]

	code, acceptOut, stderr := runCmd(t, "suggestions", "-db", db, "accept", sugID)
	if code != 0 {
		t.Fatalf("accept failed: %d %s", code, stderr)
	}
	if !strings.Contains(acceptOut, "accepted "+sugID) {
		t.Fatalf("unexpected accept output: %s", acceptOut)
	}

	// Verify task in store now includes the note
	_, showOut, _ := runCmd(t, "exec", "-db", db, `{"intent":"list_tasks"}`)
	if !strings.Contains(showOut, "T1") {
		t.Fatalf("T1 missing from list: %s", showOut)
	}
}

// TestSuggestionsCLIEdgeStaleTaskDeletedDismissWorks verifies that when a task is deleted
// by a human, pending suggestions are marked STALE, accepting is refused, and dismissing
// succeeds cleanly.
func TestSuggestionsCLIEdgeStaleTaskDeletedDismissWorks(t *testing.T) {
	db := aiSeededDB(t)

	// Step 1: AI proposes setting priority to low on T1
	code, _, stderr := runCmd(t, "exec", "-as-ai", "-by", "lidia-agent", "-db", db, `{"intent":"update_task","task_ref":"T1","priority":"low"}`)
	if code != 0 {
		t.Fatalf("exec -as-ai failed: %d %s", code, stderr)
	}

	// Step 2: Grab suggestion ID
	code, listOut, _ := runCmd(t, "suggestions", "-db", db)
	if code != 0 || !strings.Contains(listOut, "on T1") {
		t.Fatalf("unexpected list output: %s", listOut)
	}
	sugID := strings.Fields(listOut)[0]

	// Step 3: Human deletes T1
	code, delOut, delErr := runCmd(t, "task", "-db", db, "delete", "T1")
	if code != 0 {
		t.Fatalf("task delete failed: %d %s %s", code, delOut, delErr)
	}

	// Step 4: Suggestions list shows suggestion is pending
	code, listAfterDel, _ := runCmd(t, "suggestions", "-db", db)
	if code != 0 || !strings.Contains(listAfterDel, sugID) {
		t.Fatalf("expected suggestion in list: %s", listAfterDel)
	}

	// Step 5: Attempting to accept fails because task was deleted
	code, _, acceptErr := runCmd(t, "suggestions", "-db", db, "accept", sugID)
	if code != 1 || !strings.Contains(acceptErr, "not found") {
		t.Fatalf("expected accept to fail with not found on deleted task, got code %d: %s", code, acceptErr)
	}

	// Step 6: Dismissing it SUCCEEDS cleanly
	code, dismissOut, dismissErr := runCmd(t, "suggestions", "-db", db, "dismiss", sugID)
	if code != 0 {
		t.Fatalf("dismiss failed on deleted task: %d %s %s", code, dismissOut, dismissErr)
	}
	if !strings.Contains(dismissOut, "dismissed "+sugID) || !strings.Contains(dismissOut, "T1 is unchanged") {
		t.Fatalf("unexpected dismiss message: %s", dismissOut)
	}

	// Step 7: Suggestions list is now empty
	code, listEmpty, _ := runCmd(t, "suggestions", "-db", db)
	if code != 0 || !strings.Contains(listEmpty, "no suggestions") {
		t.Fatalf("expected no suggestions after dismissal, got: %s", listEmpty)
	}
}

// TestSuggestionsCLIEdgeEmptyDatabase tests CLI behavior against an empty database.
func TestSuggestionsCLIEdgeEmptyDatabase(t *testing.T) {
	t.Setenv("OFFGRID_DB", "")
	db := filepath.Join(t.TempDir(), "empty.db")
	// Initialize empty DB via exec list_tasks
	code, _, errOut := runCmd(t, "exec", "-db", db, `{"intent":"list_tasks"}`)
	if code != 0 {
		t.Fatalf("empty db init failed: %d %s", code, errOut)
	}

	// List on empty DB returns "no suggestions"
	code, out, _ := runCmd(t, "suggestions", "-db", db)
	if code != 0 || !strings.Contains(out, "no suggestions") {
		t.Fatalf("expected 'no suggestions', got %d: %s", code, out)
	}

	// JSON list on empty DB returns []
	code, out, _ = runCmd(t, "suggestions", "-db", db, "-json")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("expected '[]', got %d: %s", code, out)
	}
}

// TestSuggestionsCLIEdgeHelpAndFlagParsing tests help flags and usage messages.
func TestSuggestionsCLIEdgeHelpAndFlagParsing(t *testing.T) {
	// Help flag
	code, out, errOut := runCmd(t, "suggestions", "-h")
	if code != 0 && code != 2 {
		t.Fatalf("unexpected code for -h: %d", code)
	}
	combined := out + errOut
	if !strings.Contains(combined, "offgrid suggestions shows what an AI proposed") {
		t.Fatalf("help text missing in -h output: %s", combined)
	}

	// Unknown flag
	code, _, errOut = runCmd(t, "suggestions", "-nonexistent-flag")
	if code != 2 {
		t.Fatalf("expected code 2 for bad flag, got: %d", code)
	}
}
