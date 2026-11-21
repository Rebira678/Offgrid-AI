package sqlstore

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"offgrid/internal/task"
)

// The kill -9 test. The test binary re-executes itself as a child (TestMain sees killChildEnv) that
// writes tasks from several goroutines and prints one line per ACKNOWLEDGED write. The parent
// SIGKILLs it at a random moment, over and over on the same database, and then checks:
//   - the database opens and Verify/VerifyChain pass;
//   - every write the child printed as acknowledged is present;
//   - nothing else is present except writes that were in flight (at most one per writer);
//   - each writer's tasks form a gap-free prefix (its writes were sequential).
// SIGKILL stops the process, not the kernel: the page cache survives, so this proves the process
// side (atomic commits, ack only after COMMIT). The power-pull tool covers the disk side.

const (
	killChildEnv  = "OFFGRID_KILL9_CHILD"
	killWriters   = 6
	killRoundEnv  = "OFFGRID_KILL9_ROUND"
	killWindowEnv = "OFFGRID_KILL9_WINDOW_MS"
)

func killChild(path string) {
	round := os.Getenv(killRoundEnv)
	win := time.Duration(0)
	if ms := os.Getenv(killWindowEnv); ms != "" {
		var n int
		fmt.Sscan(ms, &n)
		win = time.Duration(n) * time.Millisecond
	}
	s, err := Open(context.Background(), path, Options{NodeID: "kill9", FlushWindow: win})
	if err != nil {
		fmt.Fprintln(os.Stderr, "child open:", err)
		os.Exit(3)
	}
	var wg sync.WaitGroup
	for g := 0; g < killWriters; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				title := fmt.Sprintf("r%s-g%d-%d", round, g, i)
				if _, _, err := s.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityLow}); err != nil {
					fmt.Fprintln(os.Stderr, "child write:", err)
					os.Exit(4)
				}
				fmt.Fprintf(os.Stdout, "ack %s\n", title) // unbuffered: one write(2) per line
			}
		}(g)
	}
	wg.Wait()
	os.Exit(0)
}

func TestAcknowledgedWritesSurviveKillNine(t *testing.T) {
	rounds := 12
	if testing.Short() {
		rounds = 4
	}
	path := newDB(t)
	rng := rand.New(rand.NewSource(99))
	acked := map[string]bool{}

	for r := 0; r < rounds; r++ {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), killChildEnv+"="+path, fmt.Sprintf("%s=%d", killRoundEnv, r), fmt.Sprintf("%s=%d", killWindowEnv, []int{0, 0, 2}[r%3]))
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		lines := make(chan string, 4096)
		go func() {
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		// Wait for the first ack, then let it run for a random while.
		first, ok := <-lines
		if !ok {
			t.Fatalf("round %d: child died before its first write", r)
		}
		record := func(l string) {
			if strings.HasPrefix(l, "ack ") {
				acked[strings.TrimPrefix(l, "ack ")] = true
			}
		}
		record(first)
		timer := time.After(time.Duration(5+rng.Intn(120)) * time.Millisecond)
	collect:
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					break collect
				}
				record(l)
			case <-timer:
				break collect
			}
		}
		cmd.Process.Kill()
		for l := range lines { // whatever it managed to print before dying
			record(l)
		}
		cmd.Wait()

		s, err := Open(context.Background(), path, Options{NodeID: "kill9"})
		if err != nil {
			t.Fatalf("round %d: reopen after kill -9: %v", r, err)
		}
		verifyOK(t, s)
		have := titlesOf(t, s)
		for k := range acked {
			if !have[k] {
				t.Fatalf("round %d: acknowledged write %s is gone", r, k)
			}
		}
		extra := 0
		for k := range have {
			if !acked[k] {
				extra++
			}
		}
		if extra > killWriters*(r+1) { // cumulative: each killed round may leave one in flight per writer
			t.Fatalf("round %d: %d unacknowledged writes present, more than the writers in flight allow", r, extra)
		}
		// per-writer prefix: r<round>-g<g>-0..m-1 with no holes
		for rr := 0; rr <= r; rr++ {
			for g := 0; g < killWriters; g++ {
				n := 0
				for have[fmt.Sprintf("r%d-g%d-%d", rr, g, n)] {
					n++
				}
				if have[fmt.Sprintf("r%d-g%d-%d", rr, g, n+1)] {
					t.Fatalf("round %d: writer %d of round %d has a hole at %d", r, g, rr, n)
				}
			}
		}
		s.Close()
	}
	if len(acked) == 0 {
		t.Fatal("no writes acknowledged; proved nothing")
	}
	t.Logf("%d rounds, %d acknowledged writes, all present", rounds, len(acked))
}
