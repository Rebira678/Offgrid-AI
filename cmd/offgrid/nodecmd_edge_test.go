package main

import (
	"bytes"
	"flag"
	"net/http"
	"runtime/debug"
	"strings"
	"testing"
)

func TestNodeFlagsPrecedenceAndGOMEMLIMIT(t *testing.T) {
	old := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(old)

	// 1. Explicit GOMEMLIMIT beats profile and flag
	debug.SetMemoryLimit(128 << 20)
	t.Setenv("GOMEMLIMIT", "128MiB")
	fs := flag.NewFlagSet("test_node", flag.ContinueOnError)
	nf := addNodeFlags(fs)
	_ = fs.Parse([]string{"-profile", "full", "-mem-limit", "512"})

	var b bytes.Buffer
	stop, code := nf.start(&b, "test")
	if code != 0 {
		t.Fatalf("expected code 0, got %d: %s", code, b.String())
	}
	defer stop()

	out := b.String()
	if !strings.Contains(out, "GOMEMLIMIT") {
		t.Errorf("output must state GOMEMLIMIT was the source: %s", out)
	}
}

func TestNodeFlagsValidationEdges(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantErrSub string
	}{
		{
			name:       "negative-slots",
			args:       []string{"-model-slots", "-1"},
			wantCode:   2,
			wantErrSub: "must not be negative",
		},
		{
			name:       "negative-mem-limit",
			args:       []string{"-mem-limit", "-100"},
			wantCode:   2,
			wantErrSub: "must not be negative",
		},
		{
			name:       "invalid-profile",
			args:       []string{"-profile", "non-existent-profile"},
			wantCode:   2,
			wantErrSub: "unknown profile",
		},
		{
			name:       "non-loopback-pprof-public-ip",
			args:       []string{"-pprof", "8.8.8.8:6060"},
			wantCode:   2,
			wantErrSub: "loopback address",
		},
		{
			name:       "non-loopback-pprof-lan",
			args:       []string{"-pprof", "192.168.1.1:6060"},
			wantCode:   2,
			wantErrSub: "loopback address",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			nf := addNodeFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			stop, code := nf.start(&b, "test")
			if stop != nil {
				defer stop()
			}
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d", code, tc.wantCode)
			}
			if !strings.Contains(b.String(), tc.wantErrSub) {
				t.Errorf("stderr %q does not contain %q", b.String(), tc.wantErrSub)
			}
		})
	}
}

func TestNodeActiveGovernorLifecycle(t *testing.T) {
	fs := flag.NewFlagSet("lifecycle", flag.ContinueOnError)
	nf := addNodeFlags(fs)
	_ = fs.Parse([]string{"-profile", "light"})

	var b bytes.Buffer
	stop, code := nf.start(&b, "lifecycle")
	if code != 0 {
		t.Fatalf("code = %d: %s", code, b.String())
	}

	// Active governor should be set
	if g := activeGov.Load(); g == nil {
		t.Fatal("expected activeGov to be set")
	}

	// Stop resets active governor
	stop()
	if g := activeGov.Load(); g != nil {
		t.Fatal("expected activeGov to be cleared after stop")
	}

	// Idempotent stop
	stop()
}

func TestNodeFlagsProfileLightLocalSlotsIgnored(t *testing.T) {
	fs := flag.NewFlagSet("light_slots", flag.ContinueOnError)
	nf := addNodeFlags(fs)
	_ = fs.Parse([]string{"-profile", "light", "-model-slots", "2"})

	var b bytes.Buffer
	stop, code := nf.start(&b, "test")
	if code != 0 {
		t.Fatalf("expected code 0, got %d: %s", code, b.String())
	}
	defer stop()

	if !strings.Contains(b.String(), "note: -model-slots is ignored on the light profile") {
		t.Fatalf("expected warning note about ignored slots on light profile, got: %s", b.String())
	}
}

func TestNodeFlagsDefaults(t *testing.T) {
	fs := flag.NewFlagSet("defaults", flag.ContinueOnError)
	nf := addNodeFlags(fs)
	_ = fs.Parse([]string{})

	if *nf.profile != "full" && *nf.profile != "auto" {
		t.Errorf("unexpected default profile: %q", *nf.profile)
	}
	if *nf.memLimit != 0 || *nf.slots != 0 || *nf.allowBattery || *nf.pprof != "" {
		t.Errorf("unexpected flag defaults: mem=%d slots=%d battery=%v pprof=%q",
			*nf.memLimit, *nf.slots, *nf.allowBattery, *nf.pprof)
	}
}

func TestNodeFlagsPprofRandomPortAndShutdown(t *testing.T) {
	fs := flag.NewFlagSet("pprof_port", flag.ContinueOnError)
	nf := addNodeFlags(fs)
	_ = fs.Parse([]string{"-pprof", "127.0.0.1:0"})

	var b bytes.Buffer
	stop, code := nf.start(&b, "pprof_test")
	if code != 0 {
		t.Fatalf("expected code 0, got %d: %s", code, b.String())
	}
	defer stop()

	out := b.String()
	if !strings.Contains(out, "pprof on http://127.0.0.1:") {
		t.Fatalf("missing pprof banner in output: %s", out)
	}

	// Extract port/url and make an HTTP request
	idx := strings.Index(out, "http://")
	endIdx := strings.Index(out[idx:], "/debug/pprof/")
	if idx == -1 || endIdx == -1 {
		t.Fatalf("failed to locate pprof URL in output: %s", out)
	}
	url := out[idx : idx+endIdx+len("/debug/pprof/")]

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("failed to GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", resp.StatusCode)
	}
}
