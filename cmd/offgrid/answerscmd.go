package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"offgrid/internal/store/sqlstore"
)

// cmdAnswers lists the answers Tier 2 (deep_think) produced. They live in the
// task database as ops, so an answer a laptop computed shows up here on a phone
// after the two sync.
//
//	offgrid answers -db FILE [-json]
func cmdAnswers(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("answers", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	asJSON := fs.Bool("json", false, "print the answers as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q (answers takes only flags)\n", fs.Arg(0))
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(stderr, "error: answers live in the task database: use -db FILE or set OFFGRID_DB")
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
	list, err := st.Answers(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if *asJSON {
		if list == nil {
			list = []sqlstore.Answer{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(list); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no answers yet (queue a hard question with: offgrid agent -route ..., then run: offgrid drain)")
		return 0
	}
	for _, a := range list {
		fmt.Fprintf(stdout, "Q: %s\n", a.Question)
		for _, line := range strings.Split(strings.TrimSpace(a.Text), "\n") {
			fmt.Fprintf(stdout, "   %s\n", line)
		}
		fmt.Fprintf(stdout, "   — %s on %s, %s (%s)\n\n", a.Model, a.Node, a.At.Local().Format("2006-01-02 15:04"), a.Source)
	}
	return 0
}
