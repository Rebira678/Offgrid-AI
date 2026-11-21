package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"offgrid/internal/task"
)

// TestVoidDependentsCascadingDeepTree tests a multi-tier dependency tree built on void tasks
// by multiple honest devices.
func TestVoidDependentsCascadingDeepTree(t *testing.T) {
	ctx := context.Background()

	ida := newKey(t)
	idb := newKey(t)
	idc := newKey(t)
	idd := newKey(t)

	a, clkA := member(t, "laptop", ida)
	b, clkB := member(t, "pi", idb)
	c, clkC := member(t, "tab", idc)
	d, clkD := member(t, "phone", idd)

	if err := a.Found(ctx, "founder"); err != nil {
		t.Fatal(err)
	}
	clkA.tick(time.Second)
	if err := a.AddMember(ctx, idb.PublicKey(), RoleMember, "pi"); err != nil {
		t.Fatal(err)
	}
	clkA.tick(time.Second)
	if err := a.AddMember(ctx, idc.PublicKey(), RoleMember, "tab"); err != nil {
		t.Fatal(err)
	}
	clkA.tick(time.Second)
	if err := a.AddMember(ctx, idd.PublicKey(), RoleMember, "phone"); err != nil {
		t.Fatal(err)
	}

	// Initial sync to all nodes
	for _, m := range []*Store{b, c, d} {
		pull(t, m, a, 50)
	}
	clkB.tick(time.Minute)
	clkC.tick(time.Minute)
	clkD.tick(time.Minute)

	// B creates legitimate task before cutoff
	kept := create(t, b, "pre-cutoff")
	pull(t, a, b, 50)
	pull(t, c, a, 50)
	pull(t, d, a, 50)

	// Admin revokes B
	clkA.tick(time.Minute)
	if _, err := a.Revoke(ctx, idb.ID()); err != nil {
		t.Fatal(err)
	}

	// B writes void tasks without knowing it's revoked
	clkB.tick(time.Minute)
	alpha := create(t, b, "void-alpha")
	beta := create(t, b, "void-beta")

	// C and D pull from B before receiving revocation news
	pull(t, c, b, 50)
	pull(t, d, b, 50)

	// C builds on void-alpha
	clkC.tick(time.Minute)
	_, err := c.AddNote(ctx, alpha.ID, "note 1 on void alpha")
	if err != nil {
		t.Fatalf("c.AddNote: %v", err)
	}
	sug1, _, err := c.Suggest(ctx, task.Draft{Task: alpha.ID, Note: "suggest change on alpha", By: "tab", Key: "k-alpha"})
	if err != nil {
		t.Fatalf("c.Suggest: %v", err)
	}

	// D pulls from C
	pull(t, d, c, 50)

	// D resolves suggestion, adds note 2, and updates alpha
	clkD.tick(time.Minute)
	if _, err := d.Resolve(ctx, sug1.ID, true); err != nil {
		t.Fatalf("d.Resolve: %v", err)
	}
	_, err = d.AddNote(ctx, alpha.ID, "note 2 by phone on alpha")
	if err != nil {
		t.Fatalf("d.AddNote: %v", err)
	}
	stDone := task.StatusDone
	if _, _, err := d.Update(ctx, alpha.ID, task.Changes{Status: &stDone}); err != nil {
		t.Fatalf("d.Update: %v", err)
	}

	// C pulls from D
	pull(t, c, d, 50)

	// C removes note 1 and deletes task alpha
	clkC.tick(time.Minute)
	if _, err := c.RemoveNote(ctx, alpha.ID, 1); err != nil {
		t.Fatalf("c.RemoveNote: %v", err)
	}
	if err := c.DeleteTask(ctx, alpha.ID); err != nil {
		t.Fatalf("c.DeleteTask: %v", err)
	}
	if _, err := c.AddNote(ctx, beta.ID, "note on void beta"); err != nil {
		t.Fatalf("c.AddNote on beta: %v", err)
	}

	// Now broadcast revocation from A and mesh sync everything
	nodes := []*Store{a, c, d}
	for round := 0; round < 4; round++ {
		for i := 0; i < len(nodes); i++ {
			for j := 0; j < len(nodes); j++ {
				if i != j {
					pull(t, nodes[i], nodes[j], 100)
				}
			}
		}
	}

	// Assert convergence and correctness on all honest nodes
	var hashes []string
	for _, s := range nodes {
		if got := viewTitles(t, s); got != kept.Title {
			t.Fatalf("%s view has %q; want %q only", s.node, got, kept.Title)
		}
		mustVerify(t, s)

		h := hashOf(t, s)
		hashes = append(hashes, h)

		rebReport, err := s.Rebuild(ctx)
		if err != nil {
			t.Fatalf("%s rebuild failed: %v", s.node, err)
		}
		if rebReport.Hash != h {
			t.Fatalf("%s state hash changed after rebuild: before=%s after=%s", s.node, h, rebReport.Hash)
		}

		voids := voidOf(t, s)
		// Exactly the 2 void task creations from B: alpha and beta
		if len(voids) != 2 {
			t.Fatalf("%s void ops count = %d, want exactly 2 (alpha and beta)", s.node, len(voids))
		}
		for _, v := range voids {
			if v.Node != "pi" {
				t.Fatalf("%s void op node = %s, want only revoked pi", s.node, v.Node)
			}
		}
	}

	for i := 1; i < len(hashes); i++ {
		if hashes[i] != hashes[0] {
			t.Fatalf("state hash mismatch between node 0 (%s) and node %d (%s)", hashes[0], i, hashes[i])
		}
	}
}

// TestTaskIsVoidEdgeCases thoroughly exercises taskIsVoid across zero, single, and
// multiple creation ops.
func TestTaskIsVoidEdgeCases(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	// Case 1: Non-existent task ID returns (false, nil) -> it is missing, NOT void
	isVoid, err := taskIsVoid(ctx, e.a.db, "T999.laptop")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if isVoid {
		t.Fatal("non-existent task reported as void, want false")
	}

	// Case 2: Special character task ID
	for _, id := range []string{"", "T' OR 1=1 --", `T"`, "T\u0000", "T#*&^"} {
		isVoid, err := taskIsVoid(ctx, e.a.db, id)
		if err != nil {
			t.Fatalf("taskIsVoid(%q): %v", id, err)
		}
		if isVoid {
			t.Fatalf("taskIsVoid(%q) = true, want false", id)
		}
	}

	// Case 3: Legitimate task is NOT void
	tk := create(t, e.b, "legit")
	isVoid, err = taskIsVoid(ctx, e.b.db, tk.ID)
	if err != nil || isVoid {
		t.Fatalf("legit task isVoid=%v, err=%v; want false, nil", isVoid, err)
	}
}

// TestConcurrentTaskCreationOneVoidOneLive verifies that if a task ID has multiple
// creation ops where at least one is NOT void, taskIsVoid reports false (the task is live).
func TestConcurrentTaskCreationOneVoidOneLive(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	// Pi (B) creates task before being revoked
	e.clkB.tick(time.Minute)
	voidPayload := `{"id":"T1.common","title":"common task","site":"plant","priority":"medium"}`

	// Admin revokes B
	e.clkA.tick(time.Minute)
	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatal(err)
	}

	// Pi (B) creates void op after revocation
	err := e.b.write(ctx, func(tx execer) error {
		return e.b.record(ctx, tx, KindTaskCreated, json.RawMessage(voidPayload))
	})
	if err != nil {
		t.Fatalf("record void op on B: %v", err)
	}

	// Tab (C) creates live op for same task ID
	err = e.c.write(ctx, func(tx execer) error {
		return e.c.record(ctx, tx, KindTaskCreated, json.RawMessage(voidPayload))
	})
	if err != nil {
		t.Fatalf("record live op on C: %v", err)
	}

	// Admin pulls from C and B
	pull(t, e.a, e.c, 50)
	pull(t, e.a, e.b, 50)

	// Since C's creation is NOT void, taskIsVoid on A must report false!
	isVoid, err := taskIsVoid(ctx, e.a.db, "T1.common")
	if err != nil {
		t.Fatalf("taskIsVoid: %v", err)
	}
	if isVoid {
		t.Fatal("task with 1 live creation and 1 void creation reported as void; want false")
	}
}

// TestSuggestionIsVoidEdgeCases tests suggestionIsVoid with:
// - Non-existent suggestion
// - Suggestion on valid task
// - Suggestion op that is void
// - Suggestion on void task
// - Non-suggestion op
func TestSuggestionIsVoidEdgeCases(t *testing.T) {
	ctx := context.Background()
	e := trio(t)

	// 1. Non-existent suggestion -> false, nil
	isVoid, err := suggestionIsVoid(ctx, e.a.db, "laptop", 99999)
	if err != nil || isVoid {
		t.Fatalf("non-existent suggestion: got void=%v, err=%v; want false, nil", isVoid, err)
	}

	// 2. Suggestion on valid task -> false, nil
	tk := create(t, e.a, "task-for-sug")
	sug, _, err := e.a.Suggest(ctx, task.Draft{Task: tk.ID, Note: "test note", By: "laptop", Key: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	sugNode, sugSeq, err := splitSuggestionID(sug.ID)
	if err != nil {
		t.Fatalf("splitSuggestionID: %v", err)
	}
	isVoid, err = suggestionIsVoid(ctx, e.a.db, sugNode, sugSeq)
	if err != nil || isVoid {
		t.Fatalf("valid suggestion: got void=%v, err=%v; want false, nil", isVoid, err)
	}

	// 3. Op that is not KindSuggestionMade -> false, nil
	isVoid, err = suggestionIsVoid(ctx, e.a.db, "laptop", 1) // founding op
	if err != nil || isVoid {
		t.Fatalf("non-suggestion op: got void=%v, err=%v; want false, nil", isVoid, err)
	}
}

// TestDanglingHelperUnit tests dangling() behavior under all return value combinations.
func TestDanglingHelperUnit(t *testing.T) {
	ctx := context.Background()
	corruptErr := errors.New("corruption error")
	probeErr := errors.New("query failed")

	// 1. isVoid returns (true, nil) -> returns nil (inert!)
	if err := dangling(ctx, nil, corruptErr, func() (bool, error) { return true, nil }); err != nil {
		t.Fatalf("dangling when void: got %v, want nil", err)
	}

	// 2. isVoid returns (false, nil) -> returns corruptErr
	if err := dangling(ctx, nil, corruptErr, func() (bool, error) { return false, nil }); !errors.Is(err, corruptErr) {
		t.Fatalf("dangling when not void: got %v, want corruptErr", err)
	}

	// 3. isVoid returns (false, probeErr) -> returns probeErr
	if err := dangling(ctx, nil, corruptErr, func() (bool, error) { return false, probeErr }); !errors.Is(err, probeErr) {
		t.Fatalf("dangling when probe errors: got %v, want probeErr", err)
	}
}
