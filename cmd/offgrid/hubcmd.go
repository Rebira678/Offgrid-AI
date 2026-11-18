package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"offgrid/internal/hub"
)

const hubHelp = `offgrid hub: a relay run by the site owner (Day 43)

  offgrid hub group add  -dir DIR -group NAME -founder DEVICE_ID
  offgrid hub group list -dir DIR
  offgrid hub serve      -dir DIR -addr HOST:PORT

The hub hosts one replica per group in DIR and lets sites that cannot reach each
other sync through it: nodes use  -peer https://HOST:PORT/g/NAME  and
-peer-device HUB_DEVICE_ID (printed by "serve"). It needs its own device key
(offgrid identity init on the hub's machine).

The hub can read every task in the groups it hosts. It cannot forge one: every
op is signed by a member's key and each node checks that itself. Say so to the
people whose data it holds.`

func cmdHub(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, hubHelp)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprintln(stderr, hubHelp)
		return 0
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runHubServe(ctx, args[1:], stdout, stderr)
	case "group":
		return cmdHubGroup(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "error: unknown hub command %q (try serve or group)\n", args[0])
	return 2
}

func openHub(ctx context.Context, dir string, opt hub.Options, stderr io.Writer) (*hub.Hub, bool) {
	if dir == "" {
		fmt.Fprintln(stderr, "error: -dir is required (the folder that holds the hub's groups)")
		return nil, false
	}
	id, err := deviceIdentity()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, false
	}
	if id == nil {
		fmt.Fprintln(stderr, "error: the hub needs its own device key and there is none. Make one with: offgrid identity init")
		return nil, false
	}
	opt.Dir, opt.Identity = dir, id
	h, err := hub.New(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, false
	}
	return h, true
}

func cmdHubGroup(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "add" && args[0] != "list") {
		fmt.Fprintln(stderr, "error: use  hub group add  or  hub group list")
		return 2
	}
	fs := flag.NewFlagSet("hub group "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "the hub's folder")
	name := fs.String("group", "", "the group's name (add)")
	founder := fs.String("founder", "", "the device ID of the group's founding admin (add)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	ctx := context.Background()
	h, ok := openHub(ctx, *dir, hub.Options{}, stderr)
	if !ok {
		return 1
	}
	defer h.Close()
	if args[0] == "add" {
		if err := h.AddGroup(ctx, *name, *founder); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "group %s added; only device %s can found it. A running hub picks it up on SIGHUP (or restart it).\n", *name, *founder)
		return 0
	}
	gs, err := h.Groups(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if len(gs) == 0 {
		fmt.Fprintln(stdout, "no groups")
	}
	for _, g := range gs {
		state := "waiting for its founder"
		if g.Founded {
			state = fmt.Sprintf("%d members", g.Members)
		}
		fmt.Fprintf(stdout, "%-20s founder %s  %s, %d ops\n", g.ID, g.Founder, state, g.Ops)
	}
	return 0
}

// runHubServe serves until ctx ends. SIGHUP re-reads the group list.
func runHubServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hub serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "the hub's folder")
	addr := fs.String("addr", "127.0.0.1:8443", "address to listen on (https, mutual TLS)")
	rate := fs.Float64("rate", 20, "requests per second each group may make")
	burst := fs.Int("burst", 40, "requests a group may make at once")
	maxBody := fs.Int64("max-body", 16<<20, "largest request body in bytes")
	maxGroups := fs.Int("max-groups", 64, "most groups this hub will host")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	lg := log.New(stderr, "hub: ", log.LstdFlags)
	h, ok := openHub(ctx, *dir, hub.Options{Log: lg, RatePerSecond: *rate, RateBurst: *burst, MaxBody: *maxBody, MaxGroups: *maxGroups}, stderr)
	if !ok {
		return 1
	}
	defer h.Close()
	srv, err := h.Server()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	id, _ := deviceIdentity()
	// Scripts and tests read this line to learn the port when the address ends in :0.
	fmt.Fprintf(stdout, "offgrid hub: serving on https://%s (mutual TLS 1.3, device %s). Nodes use -peer https://%s/g/GROUP -peer-device %s\n", ln.Addr(), id.ID(), ln.Addr(), id.ID())
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(ln, "", "") }()
	for {
		select {
		case <-hup:
			n, err := h.Reload(ctx)
			if err != nil {
				lg.Printf("reload: %v", err)
			} else {
				lg.Printf("reload: %d new groups", n)
			}
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
			fmt.Fprintln(stdout, "offgrid hub: stopped")
			return 0
		case err := <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return 0
			}
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	}
}
