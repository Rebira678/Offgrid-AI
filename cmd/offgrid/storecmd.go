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
	"strconv"
	"strings"
	"syscall"
	"time"

	"offgrid/internal/store/sqlstore"
)

// cmdStore inspects and maintains the SQLite store.
//
//	offgrid store -db FILE info      what is in it
//	offgrid store -db FILE verify    check the hash chain, replay the log in a scratch database and compare
//	offgrid store -db FILE verify -anchor FILE   also check heads saved earlier (see head)
//	offgrid store -db FILE head      print the newest hash of every replica; keep it somewhere safe
//	offgrid store -db FILE rebuild   delete the tasks view and recompute it from the log
//	offgrid store -db FILE ops -n 20 show the last ops
func cmdStore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("store", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), "the SQLite file (default: $OFFGRID_DB)")
	n := fs.Int("n", 20, "with ops: how many of the most recent ops to show")
	anchor := fs.String("anchor", "", "with verify: a file of saved heads (from: offgrid store head) the log must still agree with")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sub := fs.Arg(0)
	if rest := fs.Args(); len(rest) > 1 { // flags may also follow the command: store -db F verify -anchor FILE
		if err := fs.Parse(rest[1:]); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			sub = ""
		}
	}
	if sub == "" {
		fmt.Fprintln(stderr, "usage: offgrid store -db FILE info|verify [-anchor FILE]|head|rebuild|ops [-n N]")
		return 2
	}
	switch sub {
	case "info", "verify", "head", "rebuild", "ops", "fields":
	default:
		fmt.Fprintf(stderr, "error: unknown store command %q (use info, verify, head, rebuild, ops or fields)\n", sub)
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(stderr, "error: no database given. Use -db FILE (or set OFFGRID_DB)")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := openExistingStore(ctx, *dbPath, sqlstore.Options{})
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
		if !info.NewestStamp.IsZero() {
			fmt.Fprintf(stdout, "newest timestamp   %s\n", info.NewestStamp)
		}
		if info.ClockAhead > time.Minute {
			fmt.Fprintf(stdout, "WARNING            the log's newest timestamp is %s ahead of this computer's clock.\n                   An op was once stamped by a clock that was ahead (or this clock is now behind).\n                   New ops still sort after it; set the clock right so times make sense to people.\n", info.ClockAhead.Round(time.Second))
		}

	case "verify":
		rep, err := st.Verify(ctx)
		switch {
		case err == nil:
			fmt.Fprintf(stdout, "ok: the hash chain of all %d ops matches (%d replicas)\n", rep.Ops, len(rep.ChainHeads))
			if rep.ChainRetro > 0 {
				fmt.Fprintf(stdout, "    %d of them were chained when this database was upgraded to Day 27, not when they were written:\n    an edit made before that cannot be seen.\n", rep.ChainRetro)
			}
			fmt.Fprintf(stdout, "ok: replayed %d ops into a scratch database and got the same state\n", rep.Ops)
			fmt.Fprintf(stdout, "state hash %s\n", rep.LiveHash)
			if *anchor != "" {
				anchors, perr := readAnchors(*anchor)
				if perr != nil {
					fmt.Fprintf(stderr, "error: %v\n", perr)
					return 2
				}
				if aerr := st.VerifyAnchors(ctx, anchors); aerr != nil {
					return tamperReport(stderr, aerr)
				}
				fmt.Fprintf(stdout, "ok: all %d saved heads in %s are still in the log, unchanged\n", len(anchors), *anchor)
			} else {
				fmt.Fprintln(stdout, "note: this cannot see a log rewritten from start to end. Save the heads (offgrid store head) somewhere else and check them with -anchor.")
			}
		case errors.Is(err, sqlstore.ErrTampered):
			return tamperReport(stderr, err)
		case errors.Is(err, sqlstore.ErrDiverged):
			fmt.Fprintf(stderr, "MISMATCH: %v\n", err)
			fmt.Fprintln(stderr, "The tasks view differs from what the log says. The log is the truth: run  offgrid store -db "+*dbPath+" rebuild")
			return 1
		default:
			return storeError(stderr, err)
		}

	case "head":
		if _, err := st.VerifyChain(ctx); err != nil { // never hand out heads of a chain that does not check out
			if errors.Is(err, sqlstore.ErrTampered) {
				return tamperReport(stderr, err)
			}
			return storeError(stderr, err)
		}
		heads, err := st.Heads(ctx)
		if err != nil {
			return storeError(stderr, err)
		}
		fmt.Fprintln(stdout, "# offgrid log heads: node, sequence number, hash. Keep this somewhere the log's owner cannot rewrite.")
		for _, h := range heads {
			fmt.Fprintln(stdout, h)
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

	case "fields":
		m, err := st.Fields(ctx)
		if err != nil {
			return storeError(stderr, err)
		}
		// one line per register, with the stamp and node that won it
		enc := json.NewEncoder(stdout)
		for _, k := range m.Keys() {
			e := m[k]
			if err := enc.Encode(struct {
				Key     string `json:"key"`
				Value   string `json:"value"`
				Deleted bool   `json:"deleted,omitempty"`
				HLC     string `json:"hlc"`
				Node    string `json:"node"`
			}{k, e.Value, e.Deleted, e.Stamp.String(), e.Node}); err != nil {
				return storeError(stderr, err)
			}
		}
	case "ops":
		ops, err := st.LastOps(ctx, *n)
		if err != nil {
			return storeError(stderr, err)
		}
		enc := json.NewEncoder(stdout)
		for _, op := range ops {
			// the same op, plus its timestamp in one readable piece
			if err := enc.Encode(struct {
				sqlstore.Op
				HLC string `json:"hlc"`
			}{op, op.Stamp().String()}); err != nil {
				return storeError(stderr, err)
			}
		}
	}
	return 0
}

func storeError(w io.Writer, err error) int {
	if code, ok := noDatabaseExit(w, err); ok {
		return code
	}
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

// tamperReport says that the log no longer matches its hashes, and what that
// does and does not tell you.
func tamperReport(w io.Writer, err error) int {
	fmt.Fprintf(w, "TAMPERED: %v\n", err)
	fmt.Fprintln(w, "The log does not match the hashes recorded when it was written. Nothing has been changed or repaired:")
	fmt.Fprintln(w, "the log is evidence. Compare with another replica (offgrid sync summary) or a saved copy before you do anything.")
	return 1
}

// readAnchors reads a file of "node seq hash" lines. Blank lines and lines
// starting with # are ignored.
func readAnchors(path string) ([]sqlstore.Head, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the anchor file: %v", err)
	}
	var out []sqlstore.Head
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		var seq int64
		var perr error
		if len(f) == 3 {
			seq, perr = strconv.ParseInt(f[1], 10, 64)
		}
		if len(f) != 3 || perr != nil || seq < 1 || len(f[2]) != 64 {
			return nil, fmt.Errorf("%s line %d: want \"node seq hash\" (as printed by: offgrid store head), got %q", path, i+1, line)
		}
		out = append(out, sqlstore.Head{Node: f[0], Seq: seq, Hash: f[2]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no saved heads", path)
	}
	return out, nil
}
