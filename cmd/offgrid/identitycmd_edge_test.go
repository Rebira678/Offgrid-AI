package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIdentityCmdDeepNestedInit(t *testing.T) {
	tmp := t.TempDir()
	deepKey := filepath.Join(tmp, "a", "b", "c", "d", "e", "device.key")

	code, out, errOut := runCLI(t, "", "identity", "init", "-key", deepKey)
	if code != 0 {
		t.Fatalf("identity init failed on deep path: %d\nStdout: %s\nStderr: %s", code, out, errOut)
	}

	if !strings.Contains(out, "created a new identity") {
		t.Errorf("expected created message, got: %s", out)
	}

	// Verify the file was created and can be shown
	code, showOut, showErr := runCLI(t, "", "identity", "show", "-key", deepKey)
	if code != 0 {
		t.Fatalf("identity show failed: %d\nStderr: %s", code, showErr)
	}
	if !strings.Contains(showOut, "device ID:") {
		t.Errorf("show output missing device ID: %s", showOut)
	}
}

func TestIdentityCmdShowJSONFieldValidation(t *testing.T) {
	tmp := t.TempDir()
	key := filepath.Join(tmp, "device.key")

	runCLI(t, "", "identity", "init", "-key", key)

	code, out, errOut := runCLI(t, "", "identity", "show", "-key", key, "-json")
	if code != 0 {
		t.Fatalf("identity show -json failed: %d\nStderr: %s", code, errOut)
	}

	var parsed map[string]string
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nOutput: %s", err, out)
	}

	requiredFields := []string{"id", "algorithm", "public_key", "file", "mode"}
	for _, f := range requiredFields {
		val, ok := parsed[f]
		if !ok || val == "" {
			t.Errorf("missing or empty field %q in json output", f)
		}
	}

	if !strings.HasPrefix(parsed["id"], "dev_") {
		t.Errorf("ID should start with dev_, got %q", parsed["id"])
	}
	if parsed["algorithm"] != "ecdsa-p256" {
		t.Errorf("Algorithm should be ecdsa-p256, got %q", parsed["algorithm"])
	}
	if !strings.HasPrefix(parsed["public_key"], "ecdsa-p256:") {
		t.Errorf("public_key should start with ecdsa-p256:, got %q", parsed["public_key"])
	}
}

func TestIdentityCmdDirectoryAsKeyPath(t *testing.T) {
	tmp := t.TempDir()
	dirKey := filepath.Join(tmp, "is_a_dir")
	if err := os.Mkdir(dirKey, 0o700); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runCLI(t, "", "identity", "show", "-key", dirKey)
	if code == 0 {
		t.Errorf("identity show on directory should fail, got code 0")
	}
	if !strings.Contains(errOut, "not a regular file") && !strings.Contains(errOut, "error") {
		t.Errorf("expected error message about regular file, got: %s", errOut)
	}
}

func TestIdentityCmdReadonlyFilePerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission test")
	}

	tmp := t.TempDir()
	key := filepath.Join(tmp, "device.key")

	runCLI(t, "", "identity", "init", "-key", key)

	// Chmod to 0400 (owner read-only)
	if err := os.Chmod(key, 0o400); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "", "identity", "show", "-key", key)
	if code != 0 {
		t.Fatalf("identity show on mode 0400 failed: %d\nStderr: %s", code, errOut)
	}
	if !strings.Contains(out, "mode 0400") {
		t.Errorf("expected mode 0400 in output, got: %s", out)
	}
}
