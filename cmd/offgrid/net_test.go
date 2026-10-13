package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func runNet(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(append([]string{"net"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func probeServer(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
}

func TestNetOnce(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	code, out, _ := runNet("-once", "-url", s.URL)
	if code != 0 || !strings.Contains(out, "answered in") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestNetOnceFailsWhenNothingAnswers(t *testing.T) {
	s := probeServer(204)
	url := s.URL
	s.Close()
	code, out, _ := runNet("-once", "-url", url)
	if code != 1 || !strings.Contains(out, "no answer") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestNetOnceCaptivePortalIsNotAnswer(t *testing.T) {
	s := probeServer(200) // a login page
	defer s.Close()
	code, out, _ := runNet("-once", "-url", s.URL)
	if code != 1 || !strings.Contains(out, "captive portal") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestNetWatchOnline(t *testing.T) {
	s := probeServer(204)
	defer s.Close()
	code, out, _ := runNet("-url", s.URL, "-interval", "5ms", "-for", "400ms")
	if code != 0 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	for _, want := range []string{"watching connectivity", "==> UNKNOWN -> ONLINE", "stopped; last state ONLINE"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestNetWatchOffline(t *testing.T) {
	s := probeServer(204)
	url := s.URL
	s.Close()
	_, out, _ := runNet("-url", url, "-interval", "5ms", "-for", "400ms")
	if !strings.Contains(out, "==> UNKNOWN -> OFFLINE") || !strings.Contains(out, "stopped; last state OFFLINE") {
		t.Errorf("output:\n%s", out)
	}
}

func TestNetBadUsage(t *testing.T) {
	for _, args := range [][]string{{"extra"}, {"-url", ""}, {"-down", "0"}, {"-slow", "5s", "-timeout", "1s"}, {"-bogus"}} {
		code, _, errOut := runNet(args...)
		if code != 2 {
			t.Errorf("net %v: code %d, want 2 (stderr %q)", args, code, errOut)
		}
	}
}

func TestUsageMentionsNet(t *testing.T) {
	var out bytes.Buffer
	run([]string{"help"}, &out, &out)
	if !strings.Contains(out.String(), "offgrid net") {
		t.Error("usage does not list net")
	}
}
