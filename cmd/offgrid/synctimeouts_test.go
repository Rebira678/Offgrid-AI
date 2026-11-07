package main

import (
	"net/http"
	"testing"
)

// Day 32: every phase of the sync server must be bounded, or a half-dead peer
// holds a connection open for good.
func TestSyncServerBoundsEveryPhase(t *testing.T) {
	s := newSyncServer(http.NotFoundHandler())
	for name, d := range map[string]int64{
		"ReadHeaderTimeout": int64(s.ReadHeaderTimeout),
		"ReadTimeout":       int64(s.ReadTimeout),
		"WriteTimeout":      int64(s.WriteTimeout),
		"IdleTimeout":       int64(s.IdleTimeout),
	} {
		if d <= 0 {
			t.Errorf("%s is not set", name)
		}
	}
	if s.MaxHeaderBytes <= 0 || s.MaxHeaderBytes > 1<<20 {
		t.Errorf("MaxHeaderBytes = %d", s.MaxHeaderBytes)
	}
}
