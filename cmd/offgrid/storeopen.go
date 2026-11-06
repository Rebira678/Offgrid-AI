package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// sessionClock is the clock a command runs on. Normally it is the real time. A
// fixed -now makes demos and tests repeatable. During seeding, override makes
// the first tasks look like they were created yesterday.
//
// (An earlier version froze time at the start of the run. That is harmless for
// an in-memory demo, but with a database every op of a long session would
// record the same instant, so the clock now keeps moving unless pinned.)
type sessionClock struct {
	base     func() time.Time
	override time.Time
}

func newSessionClock(fixed *time.Time) *sessionClock {
	if fixed != nil {
		t := *fixed
		return &sessionClock{base: func() time.Time { return t }}
	}
	return &sessionClock{base: time.Now}
}

func (c *sessionClock) Now() time.Time {
	if !c.override.IsZero() {
		return c.override
	}
	return c.base()
}

// openTaskStore returns the store for a run: in memory when dbPath is empty,
// otherwise the SQLite store at dbPath (created, with its folder, if needed).
func openTaskStore(ctx context.Context, dbPath string, clk *sessionClock) (task.Store, func() error, error) {
	if dbPath == "" {
		return task.NewMemStore(clk.Now), func() error { return nil }, nil
	}
	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("cannot create the folder for the database: %w", err)
		}
	}
	st, err := sqlstore.Open(ctx, dbPath, sqlstore.Options{NodeID: nodeFromEnv(), Now: clk.Now})
	if err != nil {
		return nil, nil, err
	}
	return st, st.Close, nil
}

// dbHelp is the shared description of the -db flag.
const dbHelp = "keep tasks in this SQLite file so they survive between runs (default: $OFFGRID_DB; empty means memory only)"

// nodeFromEnv is the replica name to give a NEW database: $OFFGRID_NODE, or
// empty for a random one. It names replicas in demos ("node-a", "node-b") so
// task IDs read T4.node-b instead of T4.n-9f2c41ab07de. A database that already
// has a different node ID refuses to open, because the ID is its identity.
func nodeFromEnv() string { return os.Getenv("OFFGRID_NODE") }

// exitNoDatabase is the exit code for "that database file does not exist".
// Scripts can test for it: 1 is any other failure, 2 is a usage mistake.
const exitNoDatabase = 3

// openExistingStore opens the database at path for a command that works on what
// is already stored (list, delete, summary, inspect). It never creates a file,
// a folder or a node ID: a missing database is sqlstore.ErrNoDatabase. Only the
// commands that start or join a replica (exec, agent, sync once/serve/run) may
// create one.
func openExistingStore(ctx context.Context, path string, opt sqlstore.Options) (*sqlstore.Store, error) {
	opt.MustExist = true
	if opt.NodeID == "" {
		opt.NodeID = nodeFromEnv()
	}
	return sqlstore.Open(ctx, path, opt)
}

// noDatabaseExit reports a missing database the same way for every command and
// returns exitNoDatabase. ok is false when err is something else, so the caller
// handles it as before.
func noDatabaseExit(stderr io.Writer, err error) (code int, ok bool) {
	if !errors.Is(err, sqlstore.ErrNoDatabase) {
		return 0, false
	}
	fmt.Fprintf(stderr, "error: %v\n", err)
	fmt.Fprintln(stderr, "Nothing was created. Check the path, or start a database with: offgrid exec -db FILE ...")
	return exitNoDatabase, true
}
