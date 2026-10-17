package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"offgrid/internal/netfault"
)

// faultOpts holds the -fault-* flags. They are registered on every command
// that makes network probes, so the same switch works everywhere.
type faultOpts struct {
	latency   *time.Duration
	jitter    *time.Duration
	drop      *float64
	seed      *int64
	partition *bool
	file      *string
	schedule  *string
	mode      *string
	hang      *time.Duration

	fs *flag.FlagSet
}

func addFaultFlags(fs *flag.FlagSet) *faultOpts {
	return &faultOpts{
		fs:        fs,
		latency:   fs.Duration("fault-latency", 0, "TEST: delay every network request by this much (e.g. 900ms)"),
		jitter:    fs.Duration("fault-jitter", 0, "TEST: add a random 0..this on top of -fault-latency"),
		drop:      fs.Float64("fault-drop", 0, "TEST: lose this percentage (0-100) of network requests"),
		seed:      fs.Int64("fault-seed", 1, "TEST: seed for drops and jitter; the same seed gives the same faults"),
		partition: fs.Bool("fault-partition", false, "TEST: start with the network partitioned"),
		file:      fs.String("fault-file", os.Getenv("OFFGRID_FAULT"), "TEST: control file; write 'partition' or 'ok' into it (see: offgrid fault set) to flip a running process (default: $OFFGRID_FAULT)"),
		schedule:  fs.String("fault-schedule", "", "TEST: repeating timetable, e.g. ok:5s,partition:3s"),
		mode:      fs.String("fault-mode", "refuse", "TEST: how a partition looks: refuse (fails at once, like WiFi off) or blackhole (never answers, only your timeout ends it)"),
		hang:      fs.Duration("fault-hang", 30*time.Second, "TEST: how long a dropped or blackholed request holds on if the caller has no deadline"),
	}
}

// active reports whether any fault flag was given on the command line. The
// $OFFGRID_FAULT default alone does not count: a leftover export from
// yesterday must not silently switch injection on for today's real probes.
// Once any -fault-* flag is given, though, the file named by $OFFGRID_FAULT
// is honoured like one named with -fault-file.
func (o *faultOpts) active() bool {
	on := false
	o.fs.Visit(func(f *flag.Flag) {
		if strings.HasPrefix(f.Name, "fault-") {
			on = true
		}
	})
	return on
}

// build returns the fault transport and a one-line description, or nil, "" if
// no fault flag was given.
func (o *faultOpts) build() (*netfault.Transport, string, error) {
	if !o.active() {
		return nil, "", nil
	}
	mode, err := netfault.ParseMode(*o.mode)
	if err != nil {
		return nil, "", err
	}
	var sources []netfault.Source
	var parts []string
	if *o.latency > 0 || *o.jitter > 0 {
		parts = append(parts, fmt.Sprintf("latency %s (+0..%s)", *o.latency, *o.jitter))
	}
	if *o.drop > 0 {
		parts = append(parts, fmt.Sprintf("drop %g%%", *o.drop))
	}
	if *o.partition {
		var sw netfault.Switch
		sw.Set(true)
		sources = append(sources, sw.Source())
		parts = append(parts, "partitioned from the start")
	}
	if *o.file != "" {
		sources = append(sources, netfault.FileSource(*o.file))
		parts = append(parts, "partition set by file "+*o.file)
	}
	if *o.schedule != "" {
		sch, err := netfault.ParseSchedule(*o.schedule)
		if err != nil {
			return nil, "", fmt.Errorf("-fault-schedule: %w", err)
		}
		sources = append(sources, sch.Source(time.Now))
		parts = append(parts, "schedule "+*o.schedule)
	}
	if len(sources) > 0 {
		parts = append(parts, "partition looks like "+mode.String())
	}
	if len(parts) == 0 {
		return nil, "", errors.New("a -fault-* flag was given but it injects nothing (latency, drop, partition, file and schedule are all unset)")
	}
	tr, err := netfault.New(netfault.Config{
		Latency: *o.latency, Jitter: *o.jitter, DropPercent: *o.drop,
		Partitioned: netfault.Either(sources...), Mode: mode, Hang: *o.hang, Seed: *o.seed,
	})
	if err != nil {
		return nil, "", err
	}
	return tr, strings.Join(parts, "; "), nil
}

// probeClient is the client the connectivity probes use: it never follows
// redirects (a captive portal redirects), and goes through tr when given.
func probeClient(tr http.RoundTripper) *http.Client {
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if tr != nil {
		c.Transport = tr
	}
	return c
}

// ---- offgrid fault ----------------------------------------------------------

// cmdFault holds the small tools around the switch.
//
//	offgrid fault serve [-addr A] [-for D]   a local 204 server to probe (no internet needed)
//	offgrid fault model [-addr A] [-delay D] a STAND-IN model server with canned answers, for demos and tests
//	offgrid fault set  [-file F] partition|ok   flip the partition of a running process
//	offgrid fault show [-file F]             what the control file says now
func cmdFault(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: offgrid fault serve|model|set|show ...")
		return 2
	}
	switch args[0] {
	case "serve":
		return faultServe(args[1:], stdout, stderr)
	case "model":
		return faultModel(args[1:], stdout, stderr)
	case "set":
		return faultSet(args[1:], stdout, stderr)
	case "show":
		return faultShow(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown fault command %q (want serve, model, set or show)\n", args[0])
	return 2
}

func faultServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fault serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:0", "address to listen on (port 0 picks a free one)")
	forDur := fs.Duration("for", 0, "stop after this long (default: until Ctrl-C)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *forDur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *forDur)
		defer cancel()
	}
	// The script reads this line to learn the port, so it is printed first
	// and exactly once.
	fmt.Fprintf(stdout, "listening on http://%s/generate_204\n", ln.Addr())
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func faultFile(fs *flag.FlagSet) *string {
	return fs.String("file", os.Getenv("OFFGRID_FAULT"), "the control file (default: $OFFGRID_FAULT)")
}

func faultSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fault set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := faultFile(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(stderr, "error: need the control file: -file FILE or $OFFGRID_FAULT")
		return 2
	}
	if fs.NArg() != 1 || (fs.Arg(0) != netfault.StatePartition && fs.Arg(0) != netfault.StateOK) {
		fmt.Fprintln(stderr, "usage: offgrid fault set [-file FILE] partition|ok")
		return 2
	}
	if err := netfault.WriteState(*file, fs.Arg(0) == netfault.StatePartition); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s  network %s\n", time.Now().Format("15:04:05"), map[string]string{
		netfault.StatePartition: "PARTITIONED (injected)",
		netfault.StateOK:        "restored",
	}[fs.Arg(0)])
	return 0
}

func faultShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fault show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := faultFile(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(stderr, "error: need the control file: -file FILE or $OFFGRID_FAULT")
		return 2
	}
	p, err := netfault.FileSource(*file)()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if p {
		fmt.Fprintln(stdout, netfault.StatePartition)
	} else {
		fmt.Fprintln(stdout, netfault.StateOK)
	}
	return 0
}

// printFaultStats says what the transport did, so a recording shows that the
// faults were injected and how many.
func printFaultStats(w io.Writer, tr *netfault.Transport) {
	if tr == nil {
		return
	}
	s := tr.Stats()
	fmt.Fprintf(w, "fault injection: %d requests: %d passed, %d refused or swallowed by the partition, %d dropped\n",
		s.Requests, s.Passed, s.Partitioned, s.Dropped)
}

// faultModel is a stand-in for Ollama: it speaks the same /api/chat protocol
// but answers with canned text after a delay. It exists so the kill -9 demo
// can run on any machine, with no model installed, and so each job takes long
// enough for a kill to land in the middle of it. Every answer says it is
// canned. It is NOT a language model and proves nothing about model quality.
func faultModel(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fault model", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:0", "address to listen on (port 0 picks a free one)")
	delay := fs.Duration("delay", 1500*time.Millisecond, "how long each answer takes")
	forDur := fs.Duration("for", 0, "stop after this long (default: until Ctrl-C)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *delay < 0 {
		fmt.Fprintln(stderr, "error: usage: offgrid fault model [-addr A] [-delay D] [-for D]")
		return 2
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string          `json:"model"`
			Format   json.RawMessage `json:"format"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case <-time.After(*delay):
		case <-r.Context().Done():
			return // the caller gave up (or was killed): stop work
		}
		text := "STAND-IN ANSWER (canned text from 'offgrid fault model', not a real language model)."
		if len(req.Format) > 0 && string(req.Format) != "null" {
			text = `{"intent":"unknown","title":"","task_ref":"","site":"","priority":"","status":"","when":"","note":""}`
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		enc.Encode(map[string]any{"model": req.Model, "message": map[string]string{"role": "assistant", "content": text}, "done": false})
		enc.Encode(map[string]any{"model": req.Model, "message": map[string]string{"role": "assistant", "content": ""}, "done": true, "done_reason": "stop"})
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"models":[]}`) })
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *forDur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *forDur)
		defer cancel()
	}
	fmt.Fprintf(stdout, "stand-in model listening on http://%s (canned answers, %s each)\n", ln.Addr(), *delay)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
