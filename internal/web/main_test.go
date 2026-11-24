package web

import (
	"testing"

	"go.uber.org/goleak"
)

// A goroutine that outlives its test (a server nobody shut down, a transfer nobody cancelled) is a
// leak that a node pays for once per round for as long as it runs, so any leak here fails the run.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
