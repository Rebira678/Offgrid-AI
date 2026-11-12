package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/identity"
)

// TestDay37CLIDefaultSigningAutoProvisioning verifies that writing to a database
// via the CLI automatically provisions an ECDSA P-256 device key at the default
// location if none exists, sets mode 0600, and signs every operation written.
func TestDay37CLIDefaultSigningAutoProvisioning(t *testing.T) {
	// Isolate home/config directory to guarantee no pre-existing key
	configDir := filepath.Join(t.TempDir(), "config")
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("OFFGRID_IDENTITY_KEY", "")
	t.Setenv("OFFGRID_ALLOW_UNSIGNED", "0")

	db := filepath.Join(t.TempDir(), "tasks.db")

	// Execute a task creation command
	code, out, errOut := runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Calibrate Flow Sensor"}`)
	if code != 0 {
		t.Fatalf("exec: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "Created T1") {
		t.Fatalf("expected Created T1, got: %s", out)
	}

	// Verify key file was created with 0600 permissions
	keyPath := filepath.Join(configDir, "offgrid", "device.key")
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("device key not auto-provisioned at %s: %v", keyPath, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("device key permissions = %04o, want 0600", fi.Mode().Perm())
	}

	// Load and check key format
	id, err := identity.Load(keyPath)
	if err != nil {
		t.Fatalf("Load auto-provisioned key: %v", err)
	}
	if !strings.HasPrefix(id.ID(), "dev_") {
		t.Errorf("device ID = %s, want dev_ prefix", id.ID())
	}

	// Run store verify and confirm the op is reported signed by this device
	code, vOut, vErr := runCLI(t, "", "store", "-db", db, "verify")
	if code != 0 {
		t.Fatalf("verify: exit %d\n%s%s", code, vOut, vErr)
	}
	if !strings.Contains(vOut, "ok: 1 of 1 ops are signed, by 1 device(s)") {
		t.Errorf("verify output does not report signed op:\n%s", vOut)
	}
	if !strings.Contains(vOut, id.ID()) {
		t.Errorf("verify output lacks device ID %s:\n%s", id.ID(), vOut)
	}
}
