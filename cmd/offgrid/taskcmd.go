package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

const taskHelp = `offgrid task shows and deletes tasks in a database, with no model involved.

  offgrid task -db FILE list
  offgrid task -db FILE delete REF           REF is an ID (T3, T3.node-b) or part of a title
  offgrid task -db FILE remove-note REF N    remove the Nth note shown on that task (from 1)

Deleting is deliberately not something the language model can ask for: the
agent's "unknown" intent catches "delete everything". A delete is recorded as a
fact in the log; if another replica edits the task without having seen the
delete, the task comes back when you sync (add-wins).`

func cmdTask(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, taskHelp)
		return 0
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 || *db == "" {
		fmt.Fprintln(stderr, "usage: offgrid task -db FILE list | delete REF | remove-note REF N")
		return 2
	}
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, *db, sqlstore.Options{NodeID: nodeFromEnv()})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer st.Close()

	resolve := func(ref string) (task.Task, bool) {
		all, err := st.List(ctx, task.Filter{})
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return task.Task{}, false
		}
		t, err := task.Resolve(all, ref)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return task.Task{}, false
		}
		return t, true
	}

	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "error: list takes no arguments")
			return 2
		}
		all, err := st.List(ctx, task.Filter{})
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if len(all) == 0 {
			fmt.Fprintln(stdout, "(no tasks)")
		}
		for _, t := range all {
			site := ""
			if t.Site != "" {
				site = "  site " + t.Site
			}
			fmt.Fprintf(stdout, "%-18s %-11s %-6s %s%s\n", t.ID, t.Status, t.Priority, t.Title, site)
			for i, n := range t.Notes {
				fmt.Fprintf(stdout, "    %d. %s\n", i+1, n.Text)
			}
		}
		hash, err := st.StateHash(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "state hash: %s\n", hash)
		return 0
	case "delete":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: offgrid task -db FILE delete REF")
			return 2
		}
		t, ok := resolve(rest[1])
		if !ok {
			return 1
		}
		if err := st.DeleteTask(ctx, t.ID); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "deleted %s %q\n", t.ID, t.Title)
		return 0
	case "remove-note":
		if len(rest) != 3 {
			fmt.Fprintln(stderr, "usage: offgrid task -db FILE remove-note REF N")
			return 2
		}
		n, err := strconv.Atoi(rest[2])
		if err != nil {
			fmt.Fprintf(stderr, "error: %q is not a note number\n", rest[2])
			return 2
		}
		t, ok := resolve(rest[1])
		if !ok {
			return 1
		}
		if _, err := st.RemoveNote(ctx, t.ID, n); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed note %d of %s\n", n, t.ID)
		return 0
	}
	fmt.Fprintf(stderr, "error: unknown task command %q (list, delete or remove-note)\n", rest[0])
	return 2
}
