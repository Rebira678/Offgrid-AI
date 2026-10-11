package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"offgrid/internal/store/sqlstore"
)

// cmdStore inspects and maintains the SQLite store.
//
//	offgrid store -db FILE info      what is in it
//	offgrid store -db FILE verify    replay the log in a scratch database and compare
//	offgrid store -db FILE rebuild   delete the tasks view and recompute it from the log
//	offgrid store -db FILE ops -n 20 show the last ops
func cmdStore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("store", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), "the SQLite file (default: $OFFGRID_DB)")
	n := fs.Int("n", 20, "with ops: how many of the most recent ops to show")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sub := fs.Arg(0)
	if sub == "" || fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: offgrid store -db FILE info|verify|rebuild|ops [-n N]")
		return 2
	}
	switch sub {
	case "info", "verify", "rebuild", "ops":
	default:
		fmt.Fprintf(stderr, "error: unknown store command %q (use info, verify, rebuild or ops)\n", sub)
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(stderr, "error: no database given. Use -db FILE (or set OFFGRID_DB)")
		return 2
	}
	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(stderr, "error: no database at %s (create one by running: offgrid exec -db %s ...)\n", *dbPath, *dbPath)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := sqlstore.Open(ctx, *dbPath, sqlstore.Options{})
	if err != nil {
		return storeError(stderr, err)
	}
	defer st.Close()

	switch sub {
	case "info":
		info, err := st.Info(ctx)
		if err != nil {
			return storeError(stderr, err)
		}
		fmt.Fprintf(stdout, "database           %s\n", *dbPath)
		fmt.Fprintf(stdout, "node id            %s\n", info.NodeID)
		fmt.Fprintf(stdout, "schema version     %d\n", info.SchemaVersion)
		fmt.Fprintf(stdout, "projection version %s\n", info.ProjectionVersion)
		fmt.Fprintf(stdout, "ops in the log     %d (last position %d, view applied through %d)\n", info.Ops, info.LastPos, info.AppliedPos)
		fmt.Fprintf(stdout, "tasks in the view  %d\n", info.Tasks)
		fmt.Fprintf(stdout, "state hash         %s\n", info.StateHash)

	case "verify":
		rep, err := st.Verify(ctx)
		switch {
		case err == nil:
			fmt.Fprintf(stdout, "ok: replayed %d ops into a scratch database and got the same state\n", rep.Ops)
			fmt.Fprintf(stdout, "state hash %s\n", rep.LiveHash)
		case errors.Is(err, sqlstore.ErrDiverged):
			fmt.Fprintf(stderr, "MISMATCH: %v\n", err)
			fmt.Fprintln(stderr, "The tasks view differs from what the log says. The log is the truth: run  offgrid store -db "+*dbPath+" rebuild")
			return 1
		default:
			return storeError(stderr, err)
		}

	case "rebuild":
		info, err := st.Info(ctx)
		if err != nil {
			return storeError(stderr, err)
		}
		rep, err := st.Rebuild(ctx)
		if err != nil {
			return storeError(stderr, err)
		}
		fmt.Fprintf(stdout, "rebuilt the tasks view from %d ops: %d tasks\n", rep.Ops, rep.Tasks)
		fmt.Fprintf(stdout, "before  %s\nafter   %s\n", info.StateHash, rep.Hash)
		if info.StateHash == rep.Hash {
			fmt.Fprintln(stdout, "identical: the view was already exactly what the log says")
		} else {
			fmt.Fprintln(stdout, "CHANGED: the view had drifted from the log and has been repaired")
		}

	case "ops":
		ops, err := st.LastOps(ctx, *n)
		if err != nil {
			return storeError(stderr, err)
		}
		enc := json.NewEncoder(stdout)
		for _, op := range ops {
			if err := enc.Encode(op); err != nil {
				return storeError(stderr, err)
			}
		}
	}
	return 0
}

func storeError(w io.Writer, err error) int {
	switch {
	case errors.Is(err, sqlstore.ErrCorruptLog):
		fmt.Fprintf(w, "error: %v\n\nThe op log contains something that cannot be applied. The log is never edited automatically; look at the position named above (offgrid store ops) before doing anything else.\n", err)
	case errors.Is(err, sqlstore.ErrNewerSchema):
		fmt.Fprintf(w, "error: %v\n\nUpdate Offgrid to open this database.\n", err)
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(w, "cancelled")
		return 130
	default:
		fmt.Fprintf(w, "error: %v\n", err)
	}
	return 1
}
