package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWASMEdge_ImportPurity verifies that internal/crdt and internal/hlc
// depend solely on the Go standard library or each other.
func TestWASMEdge_ImportPurity(t *testing.T) {
	pkgs := []string{"./internal/crdt", "./internal/hlc"}
	for _, pkg := range pkgs {
		cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", pkg)
		cmd.Dir = "../.."
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list failed on %s: %v", pkg, err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || line == "offgrid/internal/crdt" || line == "offgrid/internal/hlc" {
				continue
			}
			t.Errorf("package %s has non-standard/unallowed dependency: %s", pkg, line)
		}
	}
}

// TestWASMEdge_ScriptExecution verifies that scripts/wasm-check.sh executes
// cleanly in build mode.
func TestWASMEdge_ScriptExecution(t *testing.T) {
	scriptPath := filepath.Join("..", "..", "scripts", "wasm-check.sh")
	if fi, err := os.Stat(scriptPath); err != nil || (fi.Mode()&0111) == 0 {
		t.Fatalf("expected executable script at %s", scriptPath)
	}

	cmd := exec.Command(scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scripts/wasm-check.sh failed: %v\nOutput:\n%s", err, string(out))
	}

	outStr := string(out)
	if !strings.Contains(outStr, "ok: standard library only") {
		t.Errorf("expected 'ok: standard library only' in output, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "ok: builds") {
		t.Errorf("expected 'ok: builds' in output, got:\n%s", outStr)
	}
}

// TestWASMEdge_ProductSpecDocument verifies that docs/product-spec.md exists,
// is well-structured, and includes essential service level indicators.
func TestWASMEdge_ProductSpecDocument(t *testing.T) {
	specPath := filepath.Join("..", "..", "docs", "product-spec.md")
	content, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", specPath, err)
	}
	s := string(content)
	sections := []string{
		"Offgrid product spec (v1.0 beta)",
		"## Who it is for",
		"## The job to be done",
		"## Three must-have flows",
		"## What v1 will not do",
		"## Service levels",
		"## How we will know the pilot worked",
	}
	for _, sec := range sections {
		if !strings.Contains(s, sec) {
			t.Errorf("product-spec.md missing expected section: %s", sec)
		}
	}
}
