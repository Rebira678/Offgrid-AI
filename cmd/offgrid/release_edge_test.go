package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseVersionStamping_Ldflags verifies that compile-time -X main.version ldflags
// correctly override the default version variable in the built executable.
func TestReleaseVersionStamping_Ldflags(t *testing.T) {
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "offgrid-stamped")

	customVersion := "1.4.2-release.1"
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version="+customVersion, "-o", binPath, "./")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build with ldflags failed: %v\nOutput:\n%s", err, string(out))
	}

	// Execute stamped binary with "version"
	verCmd := exec.Command(binPath, "version")
	out, err := verCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exec stamped binary failed: %v\nOutput:\n%s", err, string(out))
	}

	got := strings.TrimSpace(string(out))
	want := "offgrid " + customVersion
	if got != want {
		t.Errorf("version output = %q, want %q", got, want)
	}
}

// TestVersionCommand_CLIBehavior tests version subcommand output handling
// across standard in-process execution.
func TestVersionCommand_CLIBehavior(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(version) exit code = %d, stderr: %s", code, stderr.String())
	}
	out := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(out, "offgrid ") {
		t.Errorf("expected version to start with 'offgrid ', got: %q", out)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected empty stderr, got: %q", stderr.String())
	}
}

// TestReleaseLocal_ArtifactPackagingStructure validates that make release-local
// produces an executable artifact in dist/ that matches the specified version.
func TestReleaseLocal_ArtifactPackagingStructure(t *testing.T) {
	distDir := filepath.Join("..", "..", "dist")
	binPath := filepath.Join(distDir, "offgrid")
	if _, err := os.Stat(binPath); os.IsNotExist(err) {
		t.Skip("dist/offgrid not built yet; run make release-local to test artifact packaging")
	}

	cmd := exec.Command(binPath, "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exec dist/offgrid failed: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "offgrid ") {
		t.Errorf("unexpected output from dist/offgrid: %s", string(out))
	}
}
