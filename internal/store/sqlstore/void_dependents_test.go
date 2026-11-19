package sqlstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"offgrid/internal/task"
)

// An honest device that has not heard of a revocation keeps building on the revoked
// device's work: notes, updates, suggestions, removals. Once the revocation arrives
// that work is void, and what was built on it must not turn into a "corrupt log" that
// blocks sync: it stays in the log as evidence and simply has no effect.
func TestWorkBuiltOnAVoidTaskIsInertAndDoesNotBlockSync(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	create(t, e.b, "kept")
	pull(t, e.a, e.b, 50)
	pull(t, e.c, e.a, 50)
	e.clkA.tick(time.Minute)
	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatal(err)
	}
	late := create(t, e.b, "late")
	gone := create(t, e.b, "late-too")
	pull(t, e.c, e.b, 50) // tab has not heard of the revocation

	st := task.StatusDone
	if _, _, err := e.c.Update(ctx, late.ID, task.Changes{Status: &st}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.AddNote(ctx, late.ID, "built on void work"); err != nil {
		t.Fatal(err)
	}
	sug, _, err := e.c.Suggest(ctx, task.Draft{Task: late.ID, Note: "look", By: "m", Key: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Resolve(ctx, sug.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.c.DeleteTask(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}

	// The admin knows the revocation; it now takes tab's work. This used to be refused.
	pull(t, e.a, e.c, 50)
	pull(t, e.c, e.a, 50)
	pull(t, e.a, e.c, 50)
	pull(t, e.b, e.a, 50)
	for _, s := range []*Store{e.a, e.c} {
		if got := viewTitles(t, s); got != "kept" {
			t.Fatalf("%s sees %q, want only the work before the cutoff", s.node, got)
		}
		mustVerify(t, s)
		if _, err := s.Rebuild(ctx); err != nil {
			t.Fatalf("%s: rebuild: %v", s.node, err)
		}
	}
	if hashOf(t, e.a) != hashOf(t, e.c) {
		t.Fatal("the admin and tab disagree after the revocation")
	}
	if got := len(voidOf(t, e.a)); got != 2 {
		t.Fatalf("only the revoked device's own ops are void, not what was built on them: %d", got)
	}
}

// A device that has learned it was revoked cannot keep writing: its work would be void
// everywhere, and its own view must not differ from everyone else's.
func TestADeviceThatKnowsItIsRevokedCannotWriteAndOneThatDoesNotStillCan(t *testing.T) {
	ctx := context.Background()
	e := trio(t)
	create(t, e.b, "kept")
	pull(t, e.a, e.b, 50)
	e.clkA.tick(time.Minute)
	if _, err := e.a.Revoke(ctx, e.idb.ID()); err != nil {
		t.Fatal(err)
	}
	// Not heard yet: the write goes through (it will be void once the news arrives).
	create(t, e.b, "before-news")
	pull(t, e.b, e.a, 50)
	if got := viewTitles(t, e.b); got != "kept" {
		t.Fatalf("after the news the revoked device's view must drop its late work: %q", got)
	}
	_, _, err := e.b.Create(ctx, task.NewTask{Title: "after-news", Priority: task.PriorityMedium})
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("a revoked device that knows must be refused with ErrRevoked, got %v", err)
	}
	if got := viewTitles(t, e.b); got != "kept" {
		t.Fatalf("nothing may be written: %q", got)
	}
	pull(t, e.a, e.b, 50)
	if hashOf(t, e.a) != hashOf(t, e.b) {
		t.Fatal("the revoked device and the admin disagree")
	}
	mustVerify(t, e.b)
}
