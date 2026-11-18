package hub

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHubRegistryFileCorruptionAndDuplicates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	id := newID(t)
	founderID := id.ID()

	// 1. Corrupted JSON syntax
	if err := os.WriteFile(filepath.Join(dir, "groups.json"), []byte("{bad-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, Options{Dir: dir, Identity: id}); err == nil {
		t.Fatal("expected error on corrupted groups.json")
	}

	// 2. Duplicate group entries in groups.json
	recs := []record{
		{ID: "crew", Founder: founderID, Added: time.Now()},
		{ID: "crew", Founder: founderID, Added: time.Now()},
	}
	b, _ := json.Marshal(recs)
	if err := os.WriteFile(filepath.Join(dir, "groups.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, Options{Dir: dir, Identity: id}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate group error, got: %v", err)
	}

	// 3. Invalid record in groups.json
	recsBad := []record{
		{ID: "invalid-name!", Founder: founderID, Added: time.Now()},
	}
	b, _ = json.Marshal(recsBad)
	if err := os.WriteFile(filepath.Join(dir, "groups.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, Options{Dir: dir, Identity: id}); err == nil {
		t.Fatal("expected error on invalid entry in groups.json")
	}
}

func TestHubReloadEdgeCases(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *Options) { o.MaxGroups = 2 })
	founderA := newID(t).ID()
	founderB := newID(t).ID()
	founderC := newID(t).ID()

	if err := e.hub.AddGroup(ctx, "group-a", founderA); err != nil {
		t.Fatal(err)
	}

	// Reload with no changes
	n, err := e.hub.Reload(ctx)
	if err != nil || n != 0 {
		t.Fatalf("expected 0 added on clean reload, got %d, err=%v", n, err)
	}

	// External addition to groups.json on disk
	recs, err := readRegistry(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	recs = append(recs, record{ID: "group-b", Founder: founderB, Added: time.Now().UTC()})
	if err := writeRegistry(e.dir, recs); err != nil {
		t.Fatal(err)
	}

	n, err = e.hub.Reload(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expected 1 added on reload, got %d, err=%v", n, err)
	}

	// Now at capacity (MaxGroups=2). Adding a 3rd to disk and reloading should fail cleanly.
	recs = append(recs, record{ID: "group-c", Founder: founderC, Added: time.Now().UTC()})
	if err := writeRegistry(e.dir, recs); err != nil {
		t.Fatal(err)
	}
	_, err = e.hub.Reload(ctx)
	if err == nil || !strings.Contains(err.Error(), "limit of 2 groups") {
		t.Fatalf("expected MaxGroups limit error on reload, got %v", err)
	}

	// Existing groups still fully operational
	gs, err := e.hub.Groups(ctx)
	if err != nil || len(gs) != 2 {
		t.Fatalf("groups corrupted after failed reload: %+v, %v", gs, err)
	}
}
