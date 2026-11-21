// Command powerpull is the tool for the real power-loss test.
//
//	powerpull write DB      writes tasks as fast as it can and prints "ack <title>" to stdout for
//	                        every write the store has acknowledged. Send that output to ANOTHER
//	                        machine (ssh other 'cat > acks.txt'): an ack list kept on the machine
//	                        you are about to unplug proves nothing.
//	powerpull check DB ACKS after the power comes back, opens DB and proves that every
//	                        acknowledged write is there and the store is consistent.
//
// Flags for write: -writers N (default 8), -for DURATION (0 = until power is pulled),
// -window DURATION (the store's FlushWindow, default 0).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

const node = "powerpull"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: powerpull write DB | powerpull check DB ACKS")
		return 2
	}
	switch args[0] {
	case "write":
		return write(args[1:], stdout, stderr)
	case "check":
		return check(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "powerpull: unknown command %q\n", args[0])
	return 2
}

func write(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("write", flag.ContinueOnError)
	fs.SetOutput(stderr)
	writers := fs.Int("writers", 8, "concurrent writers")
	dur := fs.Duration("for", 0, "stop after this long (0 = until killed)")
	window := fs.Duration("window", 0, "store FlushWindow")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return 2
	}
	s, err := sqlstore.Open(context.Background(), fs.Arg(0), sqlstore.Options{NodeID: node, FlushWindow: *window})
	if err != nil {
		fmt.Fprintln(stderr, "powerpull:", err)
		return 1
	}
	defer s.Close()
	run := time.Now().UnixNano()
	var mu sync.Mutex
	var stop <-chan time.Time
	if *dur > 0 {
		stop = time.After(*dur)
	}
	done := make(chan struct{})
	go func() {
		if stop != nil {
			<-stop
			close(done)
		}
	}()
	var wg sync.WaitGroup
	for g := 0; g < *writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				title := fmt.Sprintf("pp%d-g%d-%d", run, g, i)
				if _, _, err := s.Create(context.Background(), task.NewTask{Title: title, Priority: task.PriorityLow}); err != nil {
					fmt.Fprintln(stderr, "powerpull: write:", err)
					return
				}
				mu.Lock()
				fmt.Fprintf(stdout, "ack %s\n", title)
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()
	return 0
}

func check(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: powerpull check DB ACKS")
		return 2
	}
	f, err := os.Open(args[1])
	if err != nil {
		fmt.Fprintln(stderr, "powerpull:", err)
		return 1
	}
	defer f.Close()
	var acked []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// A last line cut off mid-write by the power loss has no newline-terminated "ack " + full title; a
		// truncated title is simply not found, so only count lines that parse as complete.
		if t, ok := strings.CutPrefix(sc.Text(), "ack "); ok && strings.Count(t, "-") == 2 {
			acked = append(acked, t)
		}
	}
	s, err := sqlstore.Open(context.Background(), args[0], sqlstore.Options{NodeID: node})
	if err != nil {
		fmt.Fprintln(stdout, "FAIL: the database does not open after power loss:", err)
		return 1
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.Verify(ctx); err != nil {
		fmt.Fprintln(stdout, "FAIL: Verify:", err)
		return 1
	}
	if _, err := s.VerifyChain(ctx); err != nil {
		fmt.Fprintln(stdout, "FAIL: hash chain:", err)
		return 1
	}
	all, err := s.List(ctx, task.Filter{})
	if err != nil {
		fmt.Fprintln(stdout, "FAIL: List:", err)
		return 1
	}
	have := map[string]bool{}
	for _, t := range all {
		have[t.Title] = true
	}
	lost := 0
	for _, a := range acked {
		if !have[a] {
			lost++
			if lost <= 10 {
				fmt.Fprintln(stdout, "LOST:", a)
			}
		}
	}
	extra := len(have) - len(acked) + lost
	if lost > 0 {
		fmt.Fprintf(stdout, "FAIL: %d of %d acknowledged writes are missing\n", lost, len(acked))
		return 1
	}
	fmt.Fprintf(stdout, "PASS: all %d acknowledged writes present (%d more present that were in flight or whose ack never left the machine); store verifies\n", len(acked), extra)
	return 0
}
