package sim

import (
	"os"
	"testing"
)

// TestSimSharedHistoryAndConfigDefaults verifies that Shared: true correctly
// bootstraps replicas from a shared initial task, and that Config defaults are
// populated safely.
func TestSimSharedHistoryAndConfigDefaults(t *testing.T) {
	// 1. Verify withDefaults logic
	zeroCfg := Config{}
	def := zeroCfg.withDefaults()
	if def.Replicas != 3 {
		t.Fatalf("expected 3 default replicas, got %d", def.Replicas)
	}
	if def.Steps != 80 {
		t.Fatalf("expected 80 default steps, got %d", def.Steps)
	}
	if def.Dir == "" {
		t.Fatalf("expected non-empty default Dir")
	}

	// 2. Run simulation with Shared: true
	tempDir := t.TempDir()
	sharedCfg := Config{
		Replicas: 3,
		Steps:    10,
		Dir:      tempDir,
		Shared:   true,
	}

	res := Run(999, sharedCfg)
	if res.Failure != nil {
		t.Fatalf("sim run with Shared: true failed: %v", res.Failure)
	}

	// In a shared run, at least the initial bootstrap task exists across all replicas
	if res.Ops < 1 {
		t.Fatalf("expected at least 1 op from shared history bootstrap, got %d", res.Ops)
	}
	if res.Hash == "" {
		t.Fatalf("expected non-empty state hash, got empty string")
	}

	// 3. Test minimal schedule with only Sync actions under Shared: true
	syncOnlySchedule := []Action{
		{Kind: Sync, A: 0, B: 1, N: 1},
		{Kind: Sync, A: 1, B: 2, N: 2},
		{Kind: Sync, A: 2, B: 0, N: 3},
		{Kind: Wait, A: 0, N: 4},
	}

	resSync := RunActions(42, syncOnlySchedule, sharedCfg)
	if resSync.Failure != nil {
		t.Fatalf("sync-only schedule with shared history failed: %v", resSync.Failure)
	}
	if resSync.Ops != 1 {
		t.Fatalf("expected exactly 1 op (the shared initial task), got %d", resSync.Ops)
	}
}

// TestSimKeepFilesOnFailureFlag verifies that the Keep flag controls file cleanup.
func TestSimKeepFilesOnFailureFlag(t *testing.T) {
	tempDir := t.TempDir()
	cfg := Config{
		Replicas: 2,
		Steps:    4,
		Dir:      tempDir,
		Keep:     false,
	}
	res := Run(1234, cfg)
	if res.Failure != nil {
		t.Fatalf("unexpected failure: %v", res.Failure)
	}
	// Files should be cleanly removed when Keep: false and run passes
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("reading tempDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected clean dir after pass with Keep: false, found %d entries", len(entries))
	}
}
