package sqlstore

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"
	_ "pgregory.net/rapid" // registers the -rapid.* flags used below
)

// TestMain raises rapid's default from 100 to 2000 generated cases per
// property. Pass -rapid.checks=N to choose another number (the command line wins).
func TestMain(m *testing.M) {
	if p := os.Getenv(killChildEnv); p != "" {
		killChild(p) // never returns
	}
	_ = flag.Set("rapid.checks", "2000")
	code := m.Run()
	removeFixtures()
	if code == 0 { // a store whose Close was never called leaves its writer goroutine behind
		if err := goleak.Find(); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			code = 1
		}
	}
	os.Exit(code)
}
