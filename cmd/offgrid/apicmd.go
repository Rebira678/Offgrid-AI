package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"offgrid/internal/api"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
	"offgrid/internal/web"
	"offgrid/internal/webhook"
)

const apiHelp = `offgrid api: the stable HTTP API and webhooks (Day 53).

  offgrid api serve  -db FILE [-addr 127.0.0.1:8090]
  offgrid api spec                         print the OpenAPI document
  offgrid api export -db FILE              all tasks as CSV on stdout
  offgrid api import -db FILE [-dry-run] CSV_FILE   add tasks from a CSV file (v0)
  offgrid api webhook add -db FILE -url URL [-event TYPE ...] [-include-text]
  offgrid api webhook list|remove|test|run ...

serve     /v1 is stable (it only grows); /v0 is not yet proven and may change. It listens on this machine only
          and has no login: anything that can reach 127.0.0.1 can use it. A phone reaches the same API through
          its signed-in session on "offgrid phone serve".
webhook   Hooks live in webhooks.json next to the database (-hooks FILE to change). They are set up here, never
          through the API. "add" prints the signing secret once. "run" delivers events until you stop it.
          Delivery is at least once and best effort (v0).`

func cmdAPI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, apiHelp)
		return 0
	}
	switch args[0] {
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return apiServe(ctx, args[1:], stdout, stderr)
	case "spec":
		_, _ = stdout.Write(api.Spec)
		return 0
	case "export":
		return apiExport(args[1:], stdout, stderr)
	case "import":
		return apiImport(args[1:], stdout, stderr)
	case "webhook":
		return cmdWebhook(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown api command %q\n\n%s\n", args[0], apiHelp)
	return 2
}

func openForAPI(ctx context.Context, db string, stderr io.Writer) (*sqlstore.Store, int) {
	if db == "" {
		fmt.Fprintln(stderr, "error: use -db FILE or set OFFGRID_DB")
		return nil, 2
	}
	st, err := openExistingStore(ctx, db, sqlstore.Options{})
	if err != nil {
		if code, ok := noDatabaseExit(stderr, err); ok {
			return nil, code
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return nil, 1
	}
	return st, 0
}

func apiServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("api serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	addr := fs.String("addr", "127.0.0.1:8090", "address to listen on (this machine only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := web.RequireLoopback(*addr); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", strings.Replace(err.Error(), "the dashboard has", "the API has", 1))
		return 2
	}
	st, code := openForAPI(ctx, *db, stderr)
	if st == nil {
		return code
	}
	defer st.Close()
	h, _ := api.Handler(api.Config{Store: st, AllowImport: true})
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	srv := &http.Server{Handler: api.LocalGuard(h), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	fmt.Fprintf(stdout, "offgrid api: node %s, http://%s/v1/  (this machine only; POST/PATCH need the %s: 1 header; Ctrl-C to stop)\n", st.NodeID(), ln.Addr(), api.ActionHeader)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func apiExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("api export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	st, code := openForAPI(ctx, *db, stderr)
	if st == nil {
		return code
	}
	defer st.Close()
	ts, err := st.List(ctx, task.Filter{})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := api.WriteCSV(stdout, ts); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func apiImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("api import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	dry := fs.Bool("dry-run", false, "check the file and say what would happen, change nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: offgrid api import -db FILE [-dry-run] CSV_FILE")
		return 2
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > api.MaxImportBody {
		fmt.Fprintf(stderr, "error: the file is %d bytes; the limit is %d. Split it, nothing was imported\n", fi.Size(), api.MaxImportBody)
		return 1
	}
	ctx := context.Background()
	st, code := openForAPI(ctx, *db, stderr)
	if st == nil {
		return code
	}
	defer st.Close()
	rep, err := api.ImportCSV(ctx, st, f, *dry)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	verb := "created"
	if rep.DryRun {
		verb = "would create"
	}
	fmt.Fprintf(stdout, "%s %d of %d rows; %d skipped, %d failed\n", verb, rep.Created, rep.Rows, len(rep.Skipped), len(rep.Failed))
	for _, p := range rep.Skipped {
		fmt.Fprintf(stdout, "  row %d skipped: %s\n", p.Row, p.Message)
	}
	for _, p := range rep.Failed {
		fmt.Fprintf(stdout, "  row %d failed: %s\n", p.Row, p.Message)
	}
	if len(rep.Failed) > 0 {
		return 1
	}
	return 0
}

// ---- webhooks

const webhookHelp = `offgrid api webhook (v0):

  add    -db FILE -url URL [-event TYPE ...] [-include-text] [-hooks FILE]
  list   -db FILE [-hooks FILE]
  remove -db FILE -id wh_... [-hooks FILE]
  test   -db FILE -id wh_... [-hooks FILE]       send one "ping" now and say what the receiver answered
  run    -db FILE [-hooks FILE] [-poll 2s]        deliver events until stopped

Events: ` + "task.created task.updated task.note_added task.removed suggestion.made suggestion.resolved attachment.added ping" + `
Without -event a hook gets all of them. Without -include-text an event has IDs and changed values but no
titles, notes or file names. A hook never sees what happened before it was added.`

func hooksPath(flagVal, db string) string {
	if flagVal != "" {
		return flagVal
	}
	return filepath.Join(filepath.Dir(db), "webhooks.json")
}

func cmdWebhook(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, webhookHelp)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return webhookCmd(ctx, args, stdout, stderr)
}

func webhookCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	sub := args[0]
	fs := flag.NewFlagSet("api webhook "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	hooks := fs.String("hooks", "", "the hooks file (default: webhooks.json next to the database)")
	var url, id string
	var events listFlag
	var text bool
	poll := 2 * time.Second
	switch sub {
	case "add":
		fs.StringVar(&url, "url", "", "where to POST events (http or https)")
		fs.Var(&events, "event", "an event type (repeatable; default all)")
		fs.BoolVar(&text, "include-text", false, "include titles, notes and file names in events")
	case "remove", "test":
		fs.StringVar(&id, "id", "", "the hook's ID (offgrid api webhook list)")
	case "list":
	case "run":
		fs.DurationVar(&poll, "poll", 2*time.Second, "how often to look for new changes")
	default:
		fmt.Fprintf(stderr, "unknown webhook command %q\n\n%s\n", sub, webhookHelp)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	st, code := openForAPI(ctx, *db, stderr)
	if st == nil {
		return code
	}
	defer st.Close()
	path := hooksPath(*hooks, *db)
	f, err := webhook.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fail := func(err error) int { fmt.Fprintf(stderr, "error: %v\n", err); return 1 }
	find := func() (webhook.Hook, int, bool) {
		for i, h := range f.Hooks {
			if h.ID == id {
				return h, i, true
			}
		}
		fmt.Fprintf(stderr, "error: no hook %q here; see: offgrid api webhook list\n", id)
		return webhook.Hook{}, 0, false
	}
	switch sub {
	case "add":
		info, err := st.Info(ctx)
		if err != nil {
			return fail(err)
		}
		h, err := webhook.NewHook(url, events, text, info.LastPos, time.Now())
		if err != nil {
			return fail(err)
		}
		f.Hooks = append(f.Hooks, h)
		if err := webhook.Save(path, f); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "added %s -> %s\n  secret (shown once, keep it): %s\n", h.ID, h.URL, h.Secret)
		fmt.Fprintf(stdout, "  it sees changes from now on. Run: offgrid api webhook run -db %s\n", *db)
		return 0
	case "list":
		if len(f.Hooks) == 0 {
			fmt.Fprintln(stdout, "no webhooks")
			return 0
		}
		for _, h := range f.Hooks {
			ev := "all events"
			if len(h.Events) > 0 {
				ev = strings.Join(h.Events, ",")
			}
			fmt.Fprintf(stdout, "%s  %s  %s  text=%v  secret=%s…\n", h.ID, h.URL, ev, h.IncludeText, h.Secret[:4])
		}
		return 0
	case "remove":
		_, i, ok := find()
		if !ok {
			return 1
		}
		f.Hooks = append(f.Hooks[:i:i], f.Hooks[i+1:]...)
		if err := webhook.Save(path, f); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "removed %s\n", id)
		return 0
	case "test":
		h, _, ok := find()
		if !ok {
			return 1
		}
		d := &webhook.Dispatcher{Source: st}
		ev := webhook.Event{ID: st.NodeID() + ":ping", Type: webhook.EventPing, At: time.Now().UTC(), Node: st.NodeID(), Data: map[string]any{"hook": h.ID}}
		if err := d.Send(ctx, h, ev); err != nil {
			fmt.Fprintf(stderr, "ping failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "ping delivered; the receiver answered 2xx")
		return 0
	case "run":
		d := &webhook.Dispatcher{Source: st, Path: path, Poll: poll, Logf: func(format string, a ...any) {
			fmt.Fprintf(stderr, format+"\n", a...)
		}}
		fmt.Fprintf(stdout, "offgrid webhooks: %d hook(s) from %s  (Ctrl-C to stop)\n", len(f.Hooks), path)
		if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return fail(err)
		}
		fmt.Fprintf(stdout, "stopped: %d delivered, %d given up\n", d.Delivered.Load(), d.Failed.Load())
		return 0
	}
	return 2
}
