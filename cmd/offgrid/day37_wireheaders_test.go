package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"offgrid/internal/identity"
	"offgrid/internal/syncer"
)

// TestDay37CLISyncExchangesDeviceIDHeader verifies that sync clients and servers
// transmit the X-Offgrid-Device-ID header during sync rounds.
func TestDay37CLISyncExchangesDeviceIDHeader(t *testing.T) {
	keyA := filepath.Join(t.TempDir(), "devA.key")
	if code, _, errOut := runCLI(t, "", "identity", "init", "-key", keyA); code != 0 {
		t.Fatalf("init keyA: %s", errOut)
	}
	idA, err := identity.Load(keyA)
	if err != nil {
		t.Fatal(err)
	}

	var capturedClientDeviceID string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedClientDeviceID = r.Header.Get(syncer.HeaderDeviceID)
		w.Header().Set(syncer.HeaderDeviceID, "dev_mock_server_12345")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"highest":0,"heads":[]}`)
	}))
	defer ts.Close()

	t.Setenv("OFFGRID_IDENTITY_KEY", keyA)
	t.Setenv("OFFGRID_SYNC_KEY", strings.Repeat("s", 32))
	db := filepath.Join(t.TempDir(), "client.db")
	runCLI(t, "", "exec", "-db", db, `{"intent":"create_task","title":"Client Task"}`)

	// Run sync once against the test server
	runCLI(t, "", "sync", "once", "-db", db, "-peer", ts.URL, "-timeout", "2s")

	if capturedClientDeviceID != idA.ID() {
		t.Errorf("client X-Offgrid-Device-ID header = %q, want %q", capturedClientDeviceID, idA.ID())
	}
}
