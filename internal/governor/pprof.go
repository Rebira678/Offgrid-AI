package governor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"
)

// RequireLoopback refuses an address that is not on this machine: a profiler shows memory contents and
// goroutine stacks, and must never be reachable from the network.
func RequireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("governor: %q is not host:port", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("governor: the profiler listens on this machine only: %q is not a loopback address (use 127.0.0.1:PORT)", addr)
}

// Pprof is a running profiler endpoint.
type Pprof struct {
	Addr string // the address actually bound (useful with port 0)
	srv  *http.Server
	done chan struct{}
}

// ServePprof starts net/http/pprof on its own mux (never on a mux that serves the product, and never
// on DefaultServeMux) at a loopback address. It is off unless a command asks for it with -pprof.
func ServePprof(addr string) (*Pprof, error) {
	if err := RequireLoopback(addr); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	p := &Pprof{Addr: ln.Addr().String(), done: make(chan struct{}),
		srv: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}}
	go func() {
		defer close(p.done)
		if err := p.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return
		}
	}()
	return p, nil
}

// Close stops the endpoint and waits for it.
func (p *Pprof) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := p.srv.Shutdown(ctx)
	if err != nil {
		_ = p.srv.Close()
	}
	<-p.done
	return err
}
