package sqlstore

import (
	"context"
	"fmt"
	"testing"
)

// TestDay37CheckpointAttestedMigrationIntegrity verifies that attesting an
// unsigned database signs the head of each replica and reports legacy ops as
// "trusted as of the migration, not proven", with exact device attribution.
func TestDay37CheckpointAttestedMigrationIntegrity(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)

	// Create legacy unsigned store (Day 30 style)
	clk := &clock{t: epoch}
	legacy := open(t, db, Options{NodeID: "legacy-node", Now: clk.now})
	for i := 1; i <= 5; i++ {
		if _, _, err := legacy.Create(ctx, newTask(fmt.Sprintf("Legacy Task %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	legacy.Close()

	// Open with device key and attest
	attestKey := newKey(t)
	st := open(t, db, Options{NodeID: "legacy-node", Now: clk.now, Signer: attestKey})
	defer st.Close()

	att, err := st.Attest(ctx)
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if att.By != attestKey.ID() {
		t.Errorf("attestation by %s, want %s", att.By, attestKey.ID())
	}
	if len(att.Heads) != 1 {
		t.Errorf("attested heads count: %d, want 1", len(att.Heads))
	}

	// Verify report
	vrep, err := st.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if vrep.Sigs.Signed != 1 { // 1 attestation op signed
		t.Errorf("signed ops: %d, want 1", vrep.Sigs.Signed)
	}
	if vrep.Sigs.Attested != 5 { // 5 legacy ops covered
		t.Errorf("attested ops: %d, want 5", vrep.Sigs.Attested)
	}
	if vrep.Sigs.Unsigned != 0 {
		t.Errorf("unvouched unsigned ops: %d, want 0", vrep.Sigs.Unsigned)
	}
	if len(vrep.Sigs.Attestations) != 1 || vrep.Sigs.Attestations[0].By != attestKey.ID() {
		t.Errorf("attestations report: %+v", vrep.Sigs.Attestations)
	}
}
