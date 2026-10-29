package syncer

import (
	"context"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestThreeReplicaSyncWithCollidingNumericalTaskPrefixes tests that three replicas
// (lidia-laptop, beka-phone, and field-server) each creating their first task (T1) offline
// will produce qualified IDs (T1.lidia-laptop, T1.beka-phone, T1.field-server) that
// sync without conflict or rejection over HTTP, and all three converge to the exact same state.
func TestThreeReplicaSyncWithCollidingNumericalTaskPrefixes(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t: t0}

	storeLidia := newStore(t, "lidia-laptop", clk)
	storeBeka := newStore(t, "beka-phone", clk)
	storeField := newStore(t, "field-server", clk)

	// 1. All three create their first task independently while disconnected
	clk.t = clk.t.Add(time.Second)
	tLidia, createdL, err := storeLidia.Create(ctx, task.NewTask{Title: "Inspect Well #3", Priority: task.PriorityHigh, Site: "Well"})
	if err != nil || !createdL {
		t.Fatalf("storeLidia.Create: %v", err)
	}
	if tLidia.ID != "T1.lidia-laptop" {
		t.Fatalf("expected ID T1.lidia-laptop, got %q", tLidia.ID)
	}

	clk.t = clk.t.Add(time.Second)
	tBeka, createdB, err := storeBeka.Create(ctx, task.NewTask{Title: "Replace Battery Pack", Priority: task.PriorityMedium, Site: "Power"})
	if err != nil || !createdB {
		t.Fatalf("storeBeka.Create: %v", err)
	}
	if tBeka.ID != "T1.beka-phone" {
		t.Fatalf("expected ID T1.beka-phone, got %q", tBeka.ID)
	}

	clk.t = clk.t.Add(time.Second)
	tField, createdF, err := storeField.Create(ctx, task.NewTask{Title: "Inverter Telemetry Alert", Priority: task.PriorityLow, Site: "Array"})
	if err != nil || !createdF {
		t.Fatalf("storeField.Create: %v", err)
	}
	if tField.ID != "T1.field-server" {
		t.Fatalf("expected ID T1.field-server, got %q", tField.ID)
	}

	// 2. Lidia syncs with Beka over HTTP
	clientLB, _ := serve(t, storeBeka, clk, key)
	if _, err := clientLB.Once(ctx, storeLidia); err != nil {
		t.Fatalf("sync Lidia-Beka: %v", err)
	}

	// Verify Lidia and Beka have 2 tasks
	tasksL, _ := storeLidia.List(ctx, task.Filter{})
	if len(tasksL) != 2 {
		t.Fatalf("storeLidia has %d tasks, want 2", len(tasksL))
	}
	tasksB, _ := storeBeka.List(ctx, task.Filter{})
	if len(tasksB) != 2 {
		t.Fatalf("storeBeka has %d tasks, want 2", len(tasksB))
	}

	// 3. Field server syncs with Lidia
	clientFL, _ := serve(t, storeLidia, clk, key)
	if _, err := clientFL.Once(ctx, storeField); err != nil {
		t.Fatalf("sync Field-Lidia: %v", err)
	}

	// 4. Beka syncs with Field server
	clientBF, _ := serve(t, storeField, clk, key)
	if _, err := clientBF.Once(ctx, storeBeka); err != nil {
		t.Fatalf("sync Beka-Field: %v", err)
	}

	// 5. Final pass Lidia syncs with Beka to ensure all 3 have all 3 tasks
	if _, err := clientLB.Once(ctx, storeLidia); err != nil {
		t.Fatalf("final sync Lidia-Beka: %v", err)
	}

	// Check convergence: all 3 stores must hold all 3 tasks
	for name, s := range map[string]any{
		"Lidia": storeLidia,
		"Beka":  storeBeka,
		"Field": storeField,
	} {
		st := s.(interface {
			List(context.Context, task.Filter) ([]task.Task, error)
			Get(context.Context, string) (task.Task, error)
			StateHash(context.Context) (string, error)
		})

		all, err := st.List(ctx, task.Filter{})
		if err != nil || len(all) != 3 {
			t.Fatalf("store %s has %d tasks (want 3): %v", name, len(all), err)
		}

		for _, expectedID := range []string{"T1.lidia-laptop", "T1.beka-phone", "T1.field-server"} {
			if _, err := st.Get(ctx, expectedID); err != nil {
				t.Fatalf("store %s missing task %s: %v", name, expectedID, err)
			}
		}
	}

	// State hashes across all 3 stores must be identical
	hashL, _ := storeLidia.StateHash(ctx)
	hashB, _ := storeBeka.StateHash(ctx)
	hashF, _ := storeField.StateHash(ctx)

	if hashL != hashB || hashB != hashF {
		t.Fatalf("divergent state hashes across replicas: Lidia=%s, Beka=%s, Field=%s", hashL, hashB, hashF)
	}
}
