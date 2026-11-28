package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"offgrid/internal/task"
)

func TestConcurrentTaskCreationIdempotency(t *testing.T) {
	r := newRig(t, nil)
	hdr := map[string]string{ActionHeader: "1"}

	const workers = 20
	var createdCount int32
	var okCount int32
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			var rep createReply
			rp := r.do("POST", "/v1/tasks", map[string]string{
				"title":    "concurrent pump inspect",
				"priority": "high",
			}, hdr)
			if rp.code == 200 || rp.code == 201 {
				atomic.AddInt32(&okCount, 1)
				rp.into(t, &rep)
				if rep.Created {
					atomic.AddInt32(&createdCount, 1)
				}
			} else {
				t.Errorf("unexpected status %d: %s", rp.code, rp.body)
			}
		}()
	}
	wg.Wait()

	if okCount != workers {
		t.Fatalf("expected all %d requests to succeed, got %d", workers, okCount)
	}
	if createdCount != 1 {
		t.Fatalf("expected exactly 1 task to be created, got %d", createdCount)
	}

	// Verify only 1 task is present in store
	ts, err := r.st.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected store to contain 1 task, got %d", len(ts))
	}
}

func TestConcurrentSuggestionResolutionRace(t *testing.T) {
	r := newRig(t, nil)
	tk := r.mk("test task", "site", task.PriorityLow)
	hi := task.PriorityHigh
	sg, _, err := r.st.Suggest(context.Background(), task.Draft{
		Task:     tk.ID,
		Priority: &hi,
		By:       "model",
		Key:      "model-key-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	const racers = 10
	var successCount int32
	var conflictCount int32
	var wg sync.WaitGroup
	wg.Add(racers)

	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			rp := r.do("POST", "/v1/suggestions/"+sg.ID+"/accept", nil, map[string]string{
				ActionHeader: "1",
			})
			switch rp.code {
			case 200:
				atomic.AddInt32(&successCount, 1)
			case 409:
				atomic.AddInt32(&conflictCount, 1)
			default:
				t.Errorf("unexpected code %d: %s", rp.code, rp.body)
			}
		}()
	}
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful accept, got %d", successCount)
	}
	if conflictCount != racers-1 {
		t.Fatalf("expected %d conflicts, got %d", racers-1, conflictCount)
	}
}
