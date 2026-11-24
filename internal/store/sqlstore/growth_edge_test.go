package sqlstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

func TestGrowthEmptyStore(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := open(t, newDB(t), Options{NodeID: "test-node"})
	defer s.Close()

	g, err := s.Growth(ctx, now)
	if err != nil {
		t.Fatalf("Growth on empty store failed: %v", err)
	}

	if g.Ops != 0 {
		t.Errorf("expected 0 ops, got %d", g.Ops)
	}
	if g.OpBytes != 0 {
		t.Errorf("expected 0 OpBytes, got %d", g.OpBytes)
	}
	if !g.First.IsZero() || !g.Last.IsZero() {
		t.Errorf("expected zero First/Last timestamps on empty store, got %v / %v", g.First, g.Last)
	}
	if g.ActiveDays != 0 {
		t.Errorf("expected 0 ActiveDays, got %d", g.ActiveDays)
	}
	if g.DBBytes <= 0 {
		t.Errorf("expected positive DBBytes for SQLite schema pages, got %d", g.DBBytes)
	}
	if g.Blobs != 0 || g.BlobBytes != 0 {
		t.Errorf("expected 0 blobs and 0 blob bytes, got %d / %d", g.Blobs, g.BlobBytes)
	}
}

func TestGrowthActiveDaysUTCBoundary(t *testing.T) {
	ctx := context.Background()
	// Set clock right before UTC midnight: 2026-05-10 23:59:50 UTC
	t1 := time.Date(2026, 5, 10, 23, 59, 50, 0, time.UTC)
	clk := &clock{t: t1}

	s := open(t, newDB(t), Options{NodeID: "test-node", Now: clk.now})
	defer s.Close()

	// 1. Create first task on Day 1 (23:59:50 UTC)
	tk, _, err := s.Create(ctx, task.NewTask{Title: "Day 1 Task A", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Create second task on Day 1 (23:59:55 UTC)
	clk.tick(5 * time.Second)
	if _, _, err := s.Create(ctx, task.NewTask{Title: "Day 1 Task B", Priority: task.PriorityMedium}); err != nil {
		t.Fatal(err)
	}

	g1, err := s.Growth(ctx, clk.now())
	if err != nil {
		t.Fatal(err)
	}
	if g1.ActiveDays != 1 {
		t.Errorf("expected 1 active day on Day 1, got %d", g1.ActiveDays)
	}

	// 3. Tick clock 15 seconds across midnight into Day 2 (2026-05-11 00:00:10 UTC)
	clk.tick(15 * time.Second)
	if _, err := s.AddNote(ctx, tk.ID, "Note on Day 2"); err != nil {
		t.Fatal(err)
	}

	g2, err := s.Growth(ctx, clk.now())
	if err != nil {
		t.Fatal(err)
	}
	if g2.ActiveDays != 2 {
		t.Errorf("expected 2 active days across UTC midnight, got %d", g2.ActiveDays)
	}
	if g2.Ops != 3 {
		t.Errorf("expected 3 ops, got %d", g2.Ops)
	}
	if !g2.First.Equal(t1) {
		t.Errorf("expected First=%v, got %v", t1, g2.First)
	}
	expectedLast := t1.Add(20 * time.Second)
	if !g2.Last.Equal(expectedLast) {
		t.Errorf("expected Last=%v, got %v", expectedLast, g2.Last)
	}
}

func TestGrowthWithBlobAttachments(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	blobDir := filepath.Join(t.TempDir(), "blobs")
	s := open(t, newDB(t), Options{
		NodeID:  "test-node",
		BlobDir: blobDir,
	})
	defer s.Close()

	tk, _, err := s.Create(ctx, task.NewTask{Title: "Task with attachments", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}

	data1 := jpeg(1024, 1)
	data2 := jpeg(4096, 2)

	if _, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(data1), int64(len(data1)), "image/jpeg", "file1.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(data2), int64(len(data2)), "image/jpeg", "file2.jpg"); err != nil {
		t.Fatal(err)
	}

	g, err := s.Growth(ctx, now)
	if err != nil {
		t.Fatal(err)
	}

	if g.Blobs != 2 {
		t.Errorf("expected 2 blobs, got %d", g.Blobs)
	}
	if g.BlobBytes != 1024+4096 {
		t.Errorf("expected 5120 blob bytes, got %d", g.BlobBytes)
	}
}

func TestGrowthContextCancellation(t *testing.T) {
	s := open(t, newDB(t), Options{NodeID: "test-node"})
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel context

	_, err := s.Growth(ctx, time.Now().UTC())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error, got: %v", err)
	}
}

func TestGrowthClosedStoreReturnsError(t *testing.T) {
	s := open(t, newDB(t), Options{NodeID: "test-node"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := s.Growth(context.Background(), time.Now().UTC())
	if err == nil {
		t.Error("expected error calling Growth on closed store, got nil")
	}
}

func TestGrowthUnderConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	s := open(t, newDB(t), Options{NodeID: "concurr-node"})
	defer s.Close()

	const writers = 8
	const opsPerWriter = 10
	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < opsPerWriter; i++ {
				tk, _, err := s.Create(ctx, task.NewTask{
					Title:    fmt.Sprintf("Writer %d task %d", wid, i),
					Priority: task.PriorityLow,
				})
				if err != nil {
					t.Errorf("writer create error: %v", err)
					return
				}
				_, err = s.AddNote(ctx, tk.ID, "note")
				if err != nil {
					t.Errorf("writer note error: %v", err)
					return
				}
			}
		}(w)
	}

	// Concurrent reader calls Growth in parallel
	stopReader := make(chan struct{})
	var readerWg sync.WaitGroup
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		for {
			select {
			case <-time.After(5 * time.Millisecond):
				g, err := s.Growth(ctx, time.Now().UTC())
				if err != nil {
					return
				}
				if g.Ops < 0 || g.DBBytes <= 0 {
					t.Errorf("invalid growth stats during write: %+v", g)
					return
				}
			case <-stopReader:
				return
			}
		}
	}()

	wg.Wait()
	close(stopReader)
	readerWg.Wait()

	finalGrowth, err := s.Growth(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("final Growth failed: %v", err)
	}
	expectedOps := int64(writers * opsPerWriter * 2) // create + note
	if finalGrowth.Ops != expectedOps {
		t.Errorf("expected %d ops, got %d", expectedOps, finalGrowth.Ops)
	}
}

func TestGrowthOpMixPayloadAccounting(t *testing.T) {
	ctx := context.Background()
	s := open(t, newDB(t), Options{NodeID: "mix-node"})
	defer s.Close()

	tk, _, err := s.Create(ctx, task.NewTask{Title: "Mix Task", Priority: task.PriorityHigh})
	if err != nil {
		t.Fatal(err)
	}

	note, err := s.AddNote(ctx, tk.ID, "A very detailed note explaining the issue")
	if err != nil {
		t.Fatal(err)
	}
	_ = note

	done := task.StatusDone
	if _, _, err := s.Update(ctx, tk.ID, task.Changes{Status: &done}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}

	g, err := s.Growth(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	if g.Ops != 4 { // create, note, update, delete
		t.Errorf("expected 4 ops in log, got %d", g.Ops)
	}
	if g.OpBytes <= 0 {
		t.Errorf("expected positive payload byte count, got %d", g.OpBytes)
	}
}

func TestGrowthBlobStoreDirectoryWithSubdirectories(t *testing.T) {
	ctx := context.Background()
	blobDir := filepath.Join(t.TempDir(), "blobs")
	s := open(t, newDB(t), Options{NodeID: "blob-sub-node", BlobDir: blobDir})
	defer s.Close()

	tk, _, err := s.Create(ctx, task.NewTask{Title: "Task", Priority: task.PriorityMedium})
	if err != nil {
		t.Fatal(err)
	}

	attData := jpeg(2048, 5)
	if _, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(attData), int64(len(attData)), "image/jpeg", "img.jpg"); err != nil {
		t.Fatal(err)
	}

	// Create arbitrary subdirectories and hidden files in blobDir (simulating OS or metadata files)
	subDir := filepath.Join(blobDir, "subfolder")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, ".DS_Store"), []byte("trash"), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := s.Growth(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("Growth failed with subdirectories in blob folder: %v", err)
	}
	// Walk walks top level or subdirectories; let's verify it didn't fail
	if g.Blobs < 1 {
		t.Errorf("expected at least 1 blob, got %d", g.Blobs)
	}
}
