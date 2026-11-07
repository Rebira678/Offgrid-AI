package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"offgrid/internal/doctor"
)

const doctorHelp = `offgrid doctor [-db FILE] [-listen A,B] [-min-free-mb N] [-deep] [-json]

Checks that this machine is fit to run a node, and changes nothing:
  version   the build, the Go runtime, the SQLite library
  clock     against the newest edit in the log and the date this program was installed;
            whether a time server or a hardware clock is keeping it right
  disk      free space next to the database; a warning for SD-card-only machines
  port      that the addresses Offgrid will listen on are free
  database  SQLite's own integrity check (reads every page), opened read-only
  -deep     also recompute the hash chain and the task view (what store verify does)

Exit 0 when nothing failed (warnings are risks to read), 1 when something failed, 2 for a usage mistake.`

// cmdDoctor runs the field-readiness checks.
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", os.Getenv("OFFGRID_DB"), "the database to inspect (default: $OFFGRID_DB; empty skips the database checks)")
	listen := fs.String("listen", "127.0.0.1:7878,127.0.0.1:8787", "comma-separated addresses to test (sync serve and web defaults); empty skips")
	minFree := fs.Int("min-free-mb", 50, "fail when less than this many MiB are free next to the database (warn under ten times it)")
	deep := fs.Bool("deep", false, "also recompute the hash chain and the task view")
	asJSON := fs.Bool("json", false, "print the findings as JSON")
	timeout := fs.Duration("timeout", time.Minute, "give up after this long")
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, doctorHelp)
		return 0
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q (doctor takes only flags)\n", fs.Arg(0))
		return 2
	}
	if *minFree < 1 {
		fmt.Fprintln(stderr, "error: -min-free-mb must be at least 1")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	var addrs []string
	for _, a := range strings.Split(*listen, ",") {
		if a = strings.TrimSpace(a); a != "" {
			addrs = append(addrs, a)
		}
	}
	checks := doctor.Run(ctx, doctor.Options{
		DBPath:  *db,
		Listen:  addrs,
		MinFree: uint64(*minFree) << 20,
		Deep:    *deep,
		Version: version,
	})
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(checks)
	} else {
		doctor.Print(stdout, checks)
	}
	if _, _, failed := doctor.Summarize(checks); failed > 0 {
		return 1
	}
	return 0
}
