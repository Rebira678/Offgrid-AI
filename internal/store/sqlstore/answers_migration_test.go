package sqlstore

import (
	"context"
	"testing"
)

func TestEnsureProjectionMissingAnswersRebuilds(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaAt(t, "node-lidia", epoch)
	create(t, s, "Solar Inverter Maintenance")

	if _, err := s.RecordAnswer(ctx, "k-mig-1", "Status?", "Operational", "qwen2.5:7b", "local"); err != nil {
		t.Fatal(err)
	}

	cleanHash := hashOf(t, s)

	// Simulate catastrophic corruption of projection: drop answers table directly
	if _, err := s.db.ExecContext(ctx, `DROP TABLE answers`); err != nil {
		t.Fatal(err)
	}

	// Re-run ensureProjection: it must detect present != 3 and rebuild the missing answers table from the log
	if err := s.ensureProjection(ctx); err != nil {
		t.Fatalf("ensureProjection failed after dropping answers table: %v", err)
	}

	// Verify answers table is recreated and populated
	ans, found, err := s.Answer(ctx, "k-mig-1")
	if err != nil || !found {
		t.Fatalf("answer not found after ensureProjection rebuild: found=%v err=%v", found, err)
	}
	if ans.Text != "Operational" {
		t.Fatalf("answer text mismatch after rebuild: %q", ans.Text)
	}

	rebuiltHash := hashOf(t, s)
	if rebuiltHash != cleanHash {
		t.Fatalf("hash mismatch after ensureProjection rebuild: %s vs %s", rebuiltHash, cleanHash)
	}
}
