package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestAnswersEdgeThreeNodeConvergence(t *testing.T) {
	ctx := context.Background()
	// Lidia, Beka, and Charlie replicas
	a, aclk := replicaAt(t, "node-lidia", epoch)
	b, bclk := replicaAt(t, "node-beka", epoch)
	c, cclk := replicaAt(t, "node-charlie", epoch)

	// Lidia answers at T+1m
	aclk.tick(1 * time.Minute)
	if ok, err := a.RecordAnswer(ctx, "k-edge-conv", "What is the diagnosis?", "Lidia says check pump.", "qwen2.5:7b", "local"); !ok || err != nil {
		t.Fatalf("lidia record: ok=%v err=%v", ok, err)
	}

	// Beka answers later at T+2m
	bclk.tick(2 * time.Minute)
	if ok, err := b.RecordAnswer(ctx, "k-edge-conv", "What is the diagnosis?", "Beka says check valve.", "qwen2.5:7b", "local"); !ok || err != nil {
		t.Fatalf("beka record: ok=%v err=%v", ok, err)
	}

	// Charlie answers at T+3m
	cclk.tick(3 * time.Minute)
	if ok, err := c.RecordAnswer(ctx, "k-edge-conv", "What is the diagnosis?", "Charlie says replace filter.", "deepseek-r1:70b", "cloud"); !ok || err != nil {
		t.Fatalf("charlie record: ok=%v err=%v", ok, err)
	}

	// Also give each node a unique answer
	if _, err := a.RecordAnswer(ctx, "k-lidia-only", "qA", "ansA", "m1", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RecordAnswer(ctx, "k-beka-only", "qB", "ansB", "m2", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordAnswer(ctx, "k-charlie-only", "qC", "ansC", "m3", "cloud"); err != nil {
		t.Fatal(err)
	}

	// Sync in ring: A -> B, B -> C, C -> A, and reverse
	meet(t, a, b)
	meet(t, b, c)
	meet(t, c, a)
	meet(t, a, b)
	meet(t, b, c)

	// All three replicas must hold exactly 4 answers
	// k-edge-conv MUST have Lidia's answer ("Lidia says check pump.") because she answered earliest!
	for _, node := range []*Store{a, b, c} {
		answers := answersOf(t, node)
		if len(answers) != 4 {
			t.Fatalf("node %s has %d answers, want 4: %+v", node.NodeID(), len(answers), answers)
		}
		ans, found, err := node.Answer(ctx, "k-edge-conv")
		if err != nil || !found {
			t.Fatalf("node %s missing k-edge-conv: found=%v err=%v", node.NodeID(), found, err)
		}
		if ans.Text != "Lidia says check pump." {
			t.Fatalf("node %s resolved to %q, want earliest (Lidia's answer)", node.NodeID(), ans.Text)
		}
		if ans.Source != "local" || ans.Model != "qwen2.5:7b" {
			t.Fatalf("node %s answer metadata mismatch: %+v", node.NodeID(), ans)
		}
	}

	// State hashes must be 100% identical across all 3 nodes
	hashA := hashOf(t, a)
	hashB := hashOf(t, b)
	hashC := hashOf(t, c)
	if hashA != hashB || hashB != hashC {
		t.Fatalf("state hash divergence across replicas: A=%s B=%s C=%s", hashA, hashB, hashC)
	}
}

func TestAnswersEdgeTamperingDetectionAndRebuild(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-lidia", epoch)
	create(t, s, "Generator maintenance")

	if _, err := s.RecordAnswer(ctx, "k-tamper", "How to fix gen?", "Check fuel filter.", "qwen2.5:7b", "local"); err != nil {
		t.Fatal(err)
	}
	cleanHash := hashOf(t, s)

	// Verify reports clean state initially
	rep, err := s.Verify(ctx)
	if err != nil {
		t.Fatalf("expected initial state to be valid, got err: %v", err)
	}
	if rep.LiveHash != rep.ReplayHash {
		t.Fatalf("expected initial live and replay hash to match: live=%s replay=%s", rep.LiveHash, rep.ReplayHash)
	}

	// Directly tamper with the answers projection table behind the store's back
	if _, err := s.db.ExecContext(ctx, `UPDATE answers SET text = 'MALICIOUS_TAMPER' WHERE key = 'k-tamper'`); err != nil {
		t.Fatal(err)
	}

	// Verify MUST detect the discrepancy between projection and op-log
	rep, err = s.Verify(ctx)
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("expected ErrDiverged after tampering, got err=%v rep=%+v", err, rep)
	}
	if rep.LiveHash == rep.ReplayHash {
		t.Fatalf("expected LiveHash != ReplayHash after tampering, got identical %s", rep.LiveHash)
	}

	// Rebuild must repair the state back to the cryptographic hash from the op-log
	rebuildRep, err := s.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild failed: %v", err)
	}
	if rebuildRep.Ops < 2 {
		t.Fatalf("Rebuild ops count too low: %d", rebuildRep.Ops)
	}

	// Verify again: must be clean and match original cleanHash
	rep, err = s.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify after Rebuild failed: err=%v rep=%+v", err, rep)
	}
	if rep.LiveHash != cleanHash || rep.ReplayHash != cleanHash {
		t.Fatalf("hashes after rebuild do not match clean hash %s: live=%s replay=%s", cleanHash, rep.LiveHash, rep.ReplayHash)
	}
	rebuiltHash := hashOf(t, s)
	if rebuiltHash != cleanHash {
		t.Fatalf("rebuilt hash %s != clean hash %s", rebuiltHash, cleanHash)
	}

	ans, found, err := s.Answer(ctx, "k-tamper")
	if err != nil || !found || ans.Text != "Check fuel filter." {
		t.Fatalf("answer after rebuild mismatch: found=%v ans=%+v err=%v", found, ans, err)
	}
}

func TestAnswersEdgeCorruptLogOpValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-lidia", epoch)

	testCases := []struct {
		name    string
		payload string
		wantErr string
	}{
		{
			name:    "invalid json syntax",
			payload: `{"key": "k1", broken-json`,
			wantErr: "not JSON",
		},
		{
			name:    "empty key",
			payload: `{"key": "", "question": "q", "text": "ans", "model": "m", "source": "local"}`,
			wantErr: "an answer needs a key and text",
		},
		{
			name:    "whitespace key",
			payload: `{"key": "   ", "question": "q", "text": "ans", "model": "m", "source": "local"}`,
			wantErr: "an answer needs a key and text",
		},
		{
			name:    "empty text",
			payload: `{"key": "k1", "question": "q", "text": "", "model": "m", "source": "local"}`,
			wantErr: "an answer needs a key and text",
		},
		{
			name:    "whitespace text",
			payload: `{"key": "k1", "question": "q", "text": " \t\n ", "model": "m", "source": "local"}`,
			wantErr: "an answer needs a key and text",
		},
		{
			name:    "invalid source",
			payload: `{"key": "k1", "question": "q", "text": "ans", "model": "m", "source": "satellite"}`,
			wantErr: `an answer's source must be local or cloud, not "satellite"`,
		},
		{
			name:    "unknown extra field (strict decoding)",
			payload: `{"key": "k1", "question": "q", "text": "ans", "model": "m", "source": "local", "extra": 123}`,
			wantErr: "unknown field",
		},
	}

	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			badOp := Op{
				NodeID:  fmt.Sprintf("node-bad-%d", i),
				Seq:     1,
				Kind:    KindAnswerRecorded,
				Payload: json.RawMessage(tc.payload),
				TS:      epoch.Add(time.Minute),
			}
			_, err := s.ApplyRemote(ctx, []Op{badOp})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}

	// Verify no dirty rows were inserted
	list, err := s.Answers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 answers, found %d", len(list))
	}
}

func TestAnswersEdgeLargeAndUnicodePayload(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-lidia", epoch)

	// Unicode test string: Amharic, Japanese, German, Emoji, Math symbols
	unicodeText := strings.Repeat("በአስቸኳይ የሚስተካከል 🧠⚡️ こんにちは世界 Äpfel Übergröße ∑(x_i) = 100% ", 500)
	if len(unicodeText) < 25000 {
		t.Fatalf("test text shorter than expected: %d", len(unicodeText))
	}

	ok, err := s.RecordAnswer(ctx, "k-unicode", "How is the grid?", unicodeText, "qwen2.5:7b", "local")
	if err != nil || !ok {
		t.Fatalf("record unicode answer: ok=%v err=%v", ok, err)
	}

	ans, found, err := s.Answer(ctx, "k-unicode")
	if err != nil || !found {
		t.Fatalf("failed to retrieve unicode answer: found=%v err=%v", found, err)
	}
	if ans.Text != unicodeText {
		t.Fatalf("retrieved text does not match byte-for-byte!")
	}

	// Compute state hash and verify it rebuilds cleanly with identical hash
	h1 := hashOf(t, s)
	rep, err := s.Rebuild(ctx)
	if err != nil {
		t.Fatalf("rebuild failed: %v", err)
	}
	if rep.Ops != 1 {
		t.Fatalf("rebuild ops count: %d, want 1", rep.Ops)
	}
	h2 := hashOf(t, s)
	if h1 != h2 {
		t.Fatalf("hash changed after rebuild with unicode answer: %s vs %s", h1, h2)
	}
}

func TestAnswersEdgeConcurrentRecording(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-lidia", epoch)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	// 10 goroutines race to record the SAME key
	recordedCount := 0
	var mu sync.Mutex

	for i := 0; i < 10; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			text := fmt.Sprintf("Answer from worker %d", workerID)
			rec, err := s.RecordAnswer(ctx, "k-race-shared", "shared question", text, "qwen2.5:7b", "local")
			if err != nil {
				t.Errorf("worker %d shared record error: %v", workerID, err)
				return
			}
			if rec {
				mu.Lock()
				recordedCount++
				mu.Unlock()
			}
		}()
	}

	// 10 goroutines record distinct keys
	for i := 0; i < 10; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k-distinct-%d", workerID)
			text := fmt.Sprintf("Distinct answer %d", workerID)
			rec, err := s.RecordAnswer(ctx, key, "distinct question", text, "qwen2.5:7b", "local")
			if err != nil {
				t.Errorf("worker %d distinct record error: %v", workerID, err)
				return
			}
			if !rec {
				t.Errorf("worker %d expected distinct record to return true", workerID)
			}
		}()
	}

	wg.Wait()

	if recordedCount != 1 {
		t.Fatalf("exactly 1 worker must record the shared key, but %d recorded it", recordedCount)
	}

	list, err := s.Answers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 11 { // 1 shared + 10 distinct
		t.Fatalf("expected 11 total answers, got %d", len(list))
	}
	mustVerify(t, s)
}

func TestAnswersEdgeContextCancelled(t *testing.T) {
	s, _ := replicaAt(t, "node-lidia", epoch)
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.RecordAnswer(cancelledCtx, "k", "q", "ans", "m", "local"); err == nil {
		t.Fatal("RecordAnswer succeeded with cancelled context")
	}
	if _, _, err := s.Answer(cancelledCtx, "k"); err == nil {
		t.Fatal("Answer succeeded with cancelled context")
	}
	if _, err := s.Answers(cancelledCtx); err == nil {
		t.Fatal("Answers succeeded with cancelled context")
	}
}

func TestAnswersCanonicalTieBreakingSameTimestamp(t *testing.T) {
	ctx := context.Background()
	// Replicas node-a and node-b at the exact same epoch
	a, _ := replicaAt(t, "node-lidia", epoch)
	b, _ := replicaAt(t, "node-beka", epoch)

	// Both record an answer for the same key at the exact same timestamp
	if _, err := a.RecordAnswer(ctx, "k-tie", "Tie test", "Lidia response", "qwen2.5:7b", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RecordAnswer(ctx, "k-tie", "Tie test", "Beka response", "qwen2.5:7b", "local"); err != nil {
		t.Fatal(err)
	}

	// Exchange ops in both directions
	meet(t, a, b)

	// Rebuild on both to fold in canonical order (ordered by at, node, seq)
	if _, err := a.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}

	// Because "node-beka" < "node-lidia", node-beka's answer must win deterministically on both!
	ansA, foundA, err := a.Answer(ctx, "k-tie")
	if err != nil || !foundA {
		t.Fatalf("node-lidia answer not found: %v", err)
	}
	ansB, foundB, err := b.Answer(ctx, "k-tie")
	if err != nil || !foundB {
		t.Fatalf("node-beka answer not found: %v", err)
	}

	if ansA.Text != "Beka response" || ansB.Text != "Beka response" {
		t.Fatalf("canonical tie-break failed: A=%q B=%q, want 'Beka response'", ansA.Text, ansB.Text)
	}
	if hashOf(t, a) != hashOf(t, b) {
		t.Fatalf("state hash divergence: %s != %s", hashOf(t, a), hashOf(t, b))
	}
}

func TestRebuildPreservesAnswersAcrossTaskDeletions(t *testing.T) {
	ctx := context.Background()
	s, clk := freshStore(t)

	// Step 1: Create Task 1 and Task 2
	t1, created1, err := s.Create(ctx, task.NewTask{
		Title:    "Water pump inspection",
		Site:     "Site-4",
		Priority: task.PriorityHigh,
	})
	if err != nil || !created1 {
		t.Fatalf("create t1 failed: created=%v err=%v", created1, err)
	}

	t2, created2, err := s.Create(ctx, task.NewTask{
		Title:    "Battery bank voltage check",
		Site:     "Site-4",
		Priority: task.PriorityMedium,
	})
	if err != nil || !created2 {
		t.Fatalf("create t2 failed: created=%v err=%v", created2, err)
	}

	// Step 2: Add note to Task 1 and record Answer 1
	clk.tick(5 * time.Minute)
	if _, err := s.AddNote(ctx, t1.ID, "Bearing vibration detected"); err != nil {
		t.Fatalf("add note to t1: %v", err)
	}
	if ok, err := s.RecordAnswer(ctx, "k-pump-vibe", "Why is the pump vibrating?", "Check impeller cavitation and mount alignment.", "qwen2.5:7b", "local"); !ok || err != nil {
		t.Fatalf("record answer 1: ok=%v err=%v", ok, err)
	}

	// Step 3: Record Answer 2
	clk.tick(5 * time.Minute)
	if ok, err := s.RecordAnswer(ctx, "k-battery-cell", "Cell 3 voltage is 1.95V", "Isolate and equalize cell 3 immediately.", "deepseek-r1:70b", "cloud"); !ok || err != nil {
		t.Fatalf("record answer 2: ok=%v err=%v", ok, err)
	}

	// Step 4: Delete Task 1 (tombstone)
	clk.tick(5 * time.Minute)
	if err := s.DeleteTask(ctx, t1.ID); err != nil {
		t.Fatalf("delete t1: %v", err)
	}

	// Step 5: Update Task 2 status to in_progress
	clk.tick(5 * time.Minute)
	inProg := task.StatusInProgress
	if _, _, err := s.Update(ctx, t2.ID, task.Changes{Status: &inProg}); err != nil {
		t.Fatalf("update t2 status: %v", err)
	}

	// Step 6: Record Answer 3
	clk.tick(5 * time.Minute)
	if ok, err := s.RecordAnswer(ctx, "k-inverter-temp", "Inverter heatsink is 65C", "Normal operating threshold under load.", "qwen2.5:7b", "local"); !ok || err != nil {
		t.Fatalf("record answer 3: ok=%v err=%v", ok, err)
	}

	// Snapshot live hashes and views before rebuild
	liveHash := hashOf(t, s)
	repBefore, err := s.Verify(ctx)
	if err != nil {
		t.Fatalf("verify before rebuild: %v", err)
	}
	if repBefore.LiveHash != repBefore.ReplayHash {
		t.Fatalf("divergence before rebuild: live=%s replay=%s", repBefore.LiveHash, repBefore.ReplayHash)
	}

	// Check live tasks: t1 must not be found, t2 must be found with StatusInProgress
	tasksBefore, err := s.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasksBefore) != 1 || tasksBefore[0].ID != t2.ID || tasksBefore[0].Status != task.StatusInProgress {
		t.Fatalf("tasks before rebuild mismatch: %+v", tasksBefore)
	}

	// Check live answers: exactly 3 answers
	answersBefore, err := s.Answers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(answersBefore) != 3 {
		t.Fatalf("answers before rebuild count=%d, want 3", len(answersBefore))
	}

	// Perform full rebuild from the append-only op-log
	reb, err := s.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild failed: %v", err)
	}
	if reb.Ops < 6 {
		t.Fatalf("Rebuild ops count unexpectedly low: %d", reb.Ops)
	}

	// Verify post-rebuild
	repAfter, err := s.Verify(ctx)
	if err != nil {
		t.Fatalf("verify after rebuild: %v", err)
	}
	if repAfter.LiveHash != liveHash || repAfter.ReplayHash != liveHash {
		t.Fatalf("hash changed after rebuild: got live=%s replay=%s, want %s", repAfter.LiveHash, repAfter.ReplayHash, liveHash)
	}

	tasksAfter, err := s.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasksAfter) != 1 || tasksAfter[0].ID != t2.ID || tasksAfter[0].Status != task.StatusInProgress {
		t.Fatalf("tasks after rebuild mismatch: %+v", tasksAfter)
	}

	answersAfter, err := s.Answers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(answersAfter) != 3 {
		t.Fatalf("answers after rebuild count=%d, want 3", len(answersAfter))
	}

	for i := range answersBefore {
		if answersBefore[i] != answersAfter[i] {
			t.Fatalf("answer %d changed after rebuild: before=%+v after=%+v", i, answersBefore[i], answersAfter[i])
		}
	}
}
