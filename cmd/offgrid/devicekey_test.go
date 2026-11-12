package main

import (
	"os"
	"path/filepath"
)

// Tests must not pick up the developer's real device key from their config
// folder: a key there would make every test database signed, or fail the run
// if that key file were damaged. Point the default at a place with no key.
func init() {
	if os.Getenv("OFFGRID_IDENTITY_KEY") == "" {
		os.Setenv("OFFGRID_IDENTITY_KEY", filepath.Join(os.TempDir(), "offgrid-test-no-such-device.key"))
	}
}
