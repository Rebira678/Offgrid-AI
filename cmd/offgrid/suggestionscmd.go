package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/suggest"
	"offgrid/internal/task"
)

const suggestionsHelp = `offgrid suggestions shows what an AI proposed, and lets YOU apply or drop it.

  offgrid suggestions -db FILE                 list pending suggestions
  offgrid suggestions -db FILE -all            include accepted and dismissed ones
  offgrid suggestions -db FILE accept ID       apply it (refused if the task changed since)
  offgrid suggestions -db FILE dismiss ID      drop it; the task is untouched

A model never writes an existing task. It proposes; this command is the only
way a proposal becomes a change. A suggestion marked STALE was made against a
task that a person has since edited: accept is refused, dismiss it.`

// cmdSuggestions lists, accepts and dismisses AI suggestions.
func cmdSuggestions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("suggestions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, suggestionsHelp); fs.PrintDefaults() }
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	all := fs.Bool("all", false, "list: include accepted and dismissed suggestions")
	asJSON := fs.Bool("json", false, "list: print JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sub := fs.Arg(0)
	if sub == "" {
		sub = "list"
	}
	switch sub {
	case "list":
		if fs.NArg() > 1 {
			fmt.Fprintln(stderr, "error: list takes no arguments")
			return 2
		}
	case "accept", "dismiss":
		if fs.NArg() != 2 {
			fmt.Fprintf(stderr, "error: usage: offgrid suggestions -db FILE %s ID   (IDs look like laptop.12; list shows them)\n", sub)
			return 2
		}
	default:
		fmt.Fprintf(stderr, "error: unknown subcommand %q (use list, accept or dismiss)\n", sub)
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(stderr, "error: suggestions live in the task database: use -db FILE or set OFFGRID_DB")
		return 2
	}
	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(stderr, "error: no database at %s\n", *dbPath)
		return 1
	}
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, *dbPath, sqlstore.Options{NodeID: nodeFromEnv()})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()

	if sub == "list" {
		list, err := st.Suggestions(ctx, *all)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return printSuggestions(stdout, list, *asJSON)
	}
	id := fs.Arg(1)
	t, err := st.Resolve(ctx, id, sub == "accept")
	switch {
	case err == nil:
		if sub == "accept" {
			fmt.Fprintf(stdout, "accepted %s: %s\n", id, describeTask(t))
		} else {
			fmt.Fprintf(stdout, "dismissed %s: %s is unchanged\n", id, t.ID)
		}
		return 0
	case errors.Is(err, task.ErrStale):
		fmt.Fprintf(stderr, "refused: %v\nThe task was edited after the AI proposed this, so applying it could undo that edit. Nothing changed.\nDrop it with: offgrid suggestions -db %s dismiss %s\n", err, *dbPath, id)
		return 1
	case errors.Is(err, task.ErrSuggestionNotFound), errors.Is(err, task.ErrSuggestionResolved), errors.Is(err, task.ErrNotFound):
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	default:
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
}

func describeTask(t task.Task) string {
	return fmt.Sprintf("%s [%s, %s] %s, %d notes", t.ID, t.Priority, t.Status, t.Title, len(t.Notes))
}

func printSuggestions(w io.Writer, list []task.Suggestion, asJSON bool) int {
	if asJSON {
		if list == nil {
			list = []task.Suggestion{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(list); err != nil {
			return 1
		}
		return 0
	}
	if len(list) == 0 {
		fmt.Fprintln(w, "no suggestions")
		return 0
	}
	for _, s := range list {
		flagText := ""
		switch {
		case s.State != task.SuggestionPending:
			flagText = "  [" + string(s.State) + "]"
		case s.Stale:
			flagText = "  [STALE: the task changed since; accept is refused]"
		}
		fmt.Fprintf(w, "%s  %s%s\n", s.ID, suggest.Describe(s), flagText)
		fmt.Fprintf(w, "      proposed by %s, %s ago; the task then was %s/%s\n", s.By, time.Since(s.At).Round(time.Second), s.BaseStatus, s.BasePriority)
	}
	return 0
}
