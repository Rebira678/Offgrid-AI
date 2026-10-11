package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"offgrid/internal/intent"
	"offgrid/internal/task"
	"offgrid/internal/tools"
)

// cmdExec runs commands through the tool registry with NO model involved.
//
// It is the Day 4 proof that the rules stand on their own: you write the JSON a
// model would have proposed, and the code accepts or refuses it. Without -db each
// run uses a fresh in-memory store, so a script is the unit of work: every line
// sees the effects of the lines before it. With -db (Day 6) the tasks are kept in
// a SQLite file and later runs see them.
func cmdExec(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	script := fs.String("script", "", `file of commands, one JSON object per line; "-" reads standard input. Blank lines and lines starting with # are skipped`)
	seed := fs.Bool("seed", false, "start with three tasks created yesterday, so \"yesterday\" and \"still open\" have something to find (with -db: only adds the ones not already there)")
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	nowFlag := fs.String("now", "", `pretend it is this moment, e.g. 2026-10-08T09:00 (default: the real time); makes demos repeatable`)
	asJSON := fs.Bool("json", false, "print each result as a JSON line instead of text")
	strict := fs.Bool("strict", false, "exit with status 1 if any command was refused (for scripts and CI)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	now := time.Now()
	var fixed *time.Time
	if *nowFlag != "" {
		t, err := parseNow(*nowFlag)
		if err != nil {
			fmt.Fprintf(stderr, "error: -now: %v\n", err)
			return 2
		}
		now, fixed = t, &t
	}

	// Gather the command lines: the script first, then any given as arguments.
	var lines []string
	if *script != "" {
		var r io.Reader = stdin
		if *script != "-" {
			f, err := os.Open(*script)
			if err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
			defer f.Close()
			r = f
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintf(stderr, "error: reading script: %v\n", err)
			return 1
		}
	}
	lines = append(lines, fs.Args()...)
	if len(lines) == 0 {
		fmt.Fprintln(stderr, `error: no commands given. Try: offgrid exec -seed '{"intent":"list_tasks"}'   or   offgrid exec -script examples/day4-commands.jsonl`)
		return 2
	}

	ctx := context.Background()
	clk := newSessionClock(fixed)
	store, closeStore, err := openTaskStore(ctx, *dbPath, clk)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer closeStore()
	reg := tools.NewRegistry(store, clk.Now)

	if *seed {
		clk.override = now.AddDate(0, 0, -1)
		err := seedTasks(ctx, reg)
		clk.override = time.Time{}
		if err != nil {
			fmt.Fprintf(stderr, "error: seeding: %v\n", err)
			return 1
		}
	}

	ok, refused := 0, 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "#") && !*asJSON {
				fmt.Fprintf(stdout, "\n%s\n", line)
			}
			continue
		}
		res, err := runLine(ctx, reg, line)
		if err != nil {
			refused++
		} else {
			ok++
		}
		if *asJSON {
			writeJSONResult(stdout, line, res, err)
		} else {
			writeTextResult(stdout, line, res, err)
		}
	}
	if !*asJSON {
		fmt.Fprintf(stdout, "\n%d accepted, %d refused\n", ok, refused)
	}
	if *strict && refused > 0 {
		return 1
	}
	return 0
}

// runLine decodes one JSON command strictly and executes it.
func runLine(ctx context.Context, reg *tools.Registry, line string) (tools.Result, error) {
	var cmd intent.Command
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cmd); err != nil {
		// Keep the one detail worth showing (an unknown key); Go's raw parser
		// messages about "literal null" help nobody.
		if strings.Contains(err.Error(), "unknown field") {
			return tools.Result{}, fmt.Errorf("not a valid command: %v", err)
		}
		return tools.Result{}, errors.New("not a valid command: expected one JSON object like {\"intent\":\"list_tasks\"}")
	}
	if dec.More() {
		return tools.Result{}, errors.New("not a valid command: extra text after the JSON object")
	}
	return reg.Execute(ctx, cmd)
}

func writeTextResult(w io.Writer, line string, res tools.Result, err error) {
	fmt.Fprintf(w, "> %s\n", compact(line))
	switch {
	case err == nil:
		fmt.Fprintf(w, "  ok: %s\n", indent(res.Message))
	case errors.Is(err, tools.ErrNoTool):
		fmt.Fprintf(w, "  declined: %s\n", res.Message)
	case errors.Is(err, task.ErrAmbiguous):
		fmt.Fprintf(w, "  refused (ambiguous): %v\n", err)
	case errors.Is(err, task.ErrNotFound):
		fmt.Fprintf(w, "  refused (no such task): nothing was changed\n")
	default:
		fmt.Fprintf(w, "  refused: %v\n", err)
	}
}

func writeJSONResult(w io.Writer, line string, res tools.Result, err error) {
	out := struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Changed bool   `json:"changed"`
		Message string `json:"message,omitempty"`
		Error   string `json:"error,omitempty"`
	}{Command: compact(line), OK: err == nil, Changed: res.Changed, Message: res.Message}
	if err != nil {
		out.Error = err.Error()
	}
	b, _ := json.Marshal(out) // plain strings and bools cannot fail to encode
	fmt.Fprintln(w, string(b))
}

// compact squeezes a JSON line to its shortest form for display; anything that
// is not valid JSON is shown as written, so the person sees what was refused.
func compact(line string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(line)); err != nil {
		return line
	}
	return buf.String()
}

func indent(s string) string { return strings.ReplaceAll(s, "\n", "\n  ") }

func seedTasks(ctx context.Context, reg *tools.Registry) error {
	for _, c := range []intent.Command{
		{Intent: intent.CreateTask, Title: "Leaking pump", Site: "4", Priority: "high"},
		{Intent: intent.CreateTask, Title: "Replace filter", Site: "2", Priority: "medium"},
		{Intent: intent.CreateTask, Title: "Calibrate gauge", Site: "4", Priority: "low"},
	} {
		if _, err := reg.Execute(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

func parseNow(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read %q; use a form like 2026-10-08T09:00", s)
}

// cmdTools prints what the registry offers: the complete list of things a model
// is able to ask for. There is nothing else.
func cmdTools(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tools", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	reg := tools.NewRegistry(task.NewMemStore(nil), nil)
	for i, s := range reg.Specs() {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stdout, "%s\n  %s\n", s.Intent, s.Does)
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		for _, a := range s.Args {
			req := "optional"
			if a.Required {
				req = "required"
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s\n", a.Field, req, a.Doc)
		}
		tw.Flush()
	}
	fmt.Fprintf(stdout, "\nAnything else, including intent %q, is refused and touches nothing.\n", intent.Unknown)
	return 0
}
