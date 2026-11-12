package syncer

import (
	"testing"

	"go.uber.org/goleak"
)

// Every test in this package starts servers, clients and sync loops. A
// goroutine that outlives its test (a connection nobody closed, a sync round
// nobody cancelled) is how a node leaks one goroutine per round for a week, so
// a leak anywhere here fails the whole run, and names the stack that leaked.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
