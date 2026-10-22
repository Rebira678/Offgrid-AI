package sqlstore

import (
	"flag"
	"os"
	"testing"

	_ "pgregory.net/rapid" // registers the -rapid.* flags used below
)

// TestMain raises rapid's default from 100 to 2000 generated cases per
// property. Pass -rapid.checks=N to choose another number (the command line wins).
func TestMain(m *testing.M) {
	_ = flag.Set("rapid.checks", "2000")
	os.Exit(m.Run())
}
