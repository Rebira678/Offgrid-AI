package hub

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

func TestHubHighConcurrencySyncStress(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *Options) {
		// Provide generous budget for concurrency stress test
		o.RatePerSecond = 1000
		o.RateBurst = 2000
	})

	founderID, _ := identity.Generate()
	groupName := "stress-group"
	if err := e.hub.AddGroup(ctx, groupName, founderID.ID()); err != nil {
		t.Fatal(err)
	}

	// Founder establishes group on their replica and pushes founding op
	founderStore, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "founder.db"), sqlstore.Options{
		NodeID: "founder",
		Now:    e.clk.now,
		Signer: founderID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer founderStore.Close()

	if err := founderStore.Found(ctx, "founder site"); err != nil {
		t.Fatal(err)
	}

	// Enrol 5 worker nodes into the founder's store
	workerNodes := make([]node, 5)
	for i := 0; i < len(workerNodes); i++ {
		workerNodes[i] = e.node(fmt.Sprintf("worker-%d", i))
		if err := founderStore.AddMember(ctx, workerNodes[i].id.PublicKey(), sqlstore.RoleMember, fmt.Sprintf("worker %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Founder pushes initial state (founding + membership ops) to hub
	founderClient := e.client(node{st: founderStore, id: founderID}, groupName, nil)
	if rep, err := founderClient.Once(ctx, founderStore); err != nil || rep.Pushed == 0 {
		t.Fatalf("founder push: %+v, %v", rep, err)
	}

	// Now all 5 workers sync from hub to learn membership
	for i := 0; i < len(workerNodes); i++ {
		wClient := e.client(workerNodes[i], groupName, nil)
		if rep, err := wClient.Once(ctx, workerNodes[i].st); err != nil || rep.Pulled == 0 {
			t.Fatalf("worker %d initial pull: %+v, %v", i, rep, err)
		}
	}

	// Concurrency phase: Each worker creates 10 tasks and syncs concurrently
	var wg sync.WaitGroup
	errCh := make(chan error, len(workerNodes)*10)
	tasksPerWorker := 10

	for i := 0; i < len(workerNodes); i++ {
		wIdx := i
		wNode := workerNodes[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			wClient := e.client(wNode, groupName, nil)
			for j := 0; j < tasksPerWorker; j++ {
				title := fmt.Sprintf("Task from worker %d step %d", wIdx, j)
				if _, _, err := wNode.st.Create(ctx, task.NewTask{Title: title, Priority: task.PriorityLow}); err != nil {
					errCh <- fmt.Errorf("worker %d create task %d: %w", wIdx, j, err)
					return
				}
				if _, err := wClient.Once(ctx, wNode.st); err != nil {
					errCh <- fmt.Errorf("worker %d sync task %d: %w", wIdx, j, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent sync worker error: %v", err)
	}

	// Convergence phase: All workers pull until everyone converges
	for round := 0; round < 3; round++ {
		for i := 0; i < len(workerNodes); i++ {
			wClient := e.client(workerNodes[i], groupName, nil)
			_, _ = wClient.Once(ctx, workerNodes[i].st)
		}
	}

	// Pull into founder store as well
	if _, err := founderClient.Once(ctx, founderStore); err != nil {
		t.Fatalf("founder final pull: %v", err)
	}

	// Verify all stores agree on state hash
	targetHash, err := founderStore.StateHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(workerNodes); i++ {
		h, err := workerNodes[i].st.StateHash(ctx)
		if err != nil {
			t.Fatalf("worker %d hash: %v", i, err)
		}
		if h != targetHash {
			t.Fatalf("worker %d hash %s differs from founder hash %s", i, h, targetHash)
		}
	}

	// Verify all tasks exist
	allTasks, err := founderStore.List(ctx, task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	expectedTaskCount := len(workerNodes) * tasksPerWorker
	if len(allTasks) != expectedTaskCount {
		t.Fatalf("expected %d tasks converged, got %d", expectedTaskCount, len(allTasks))
	}
}
