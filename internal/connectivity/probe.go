package connectivity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Result is the outcome of one probe.
type Result struct {
	OK      bool          // the target answered correctly
	Latency time.Duration // how long it took (set whether or not OK)
	Err     error         // why it failed, when !OK
	Target  string        // which target answered (or the last that failed)
}

// Prober checks the network once. Probe must respect ctx: when it is
// cancelled or its deadline passes, Probe returns promptly.
type Prober interface {
	Probe(ctx context.Context) Result
}

// ProberFunc adapts a function to the Prober interface.
type ProberFunc func(ctx context.Context) Result

func (f ProberFunc) Probe(ctx context.Context) Result { return f(ctx) }

// ---- HTTP ---------------------------------------------------------------

// HTTPProber fetches a URL and expects one exact status code. Connectivity
// check endpoints (Cloudflare, Google, Apple) answer 204 with no body.
//
// Exactly one status is accepted. A hotel or airport WiFi that has not been
// signed into typically answers 200 or 302 with a login page: the network
// "works" but nothing useful gets through. Treating that as success would
// make us start syncing into a login page, so it counts as a failure.
type HTTPProber struct {
	URL    string
	Expect int          // default 204
	Client *http.Client // default: a client that never follows redirects
}

func (p HTTPProber) Probe(ctx context.Context) Result {
	start := time.Now()
	res := Result{Target: p.URL}
	fail := func(err error) Result {
		res.Err = err
		res.Latency = time.Since(start)
		return res
	}

	expect := p.Expect
	if expect == 0 {
		expect = http.StatusNoContent
	}
	client := p.Client
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return fail(err)
	}
	req.Header.Set("User-Agent", "offgrid-connectivity/1")
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // let the connection be reused
	if resp.StatusCode != expect {
		hint := ""
		if resp.StatusCode == 200 || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
			hint = " (a captive portal login page?)"
		}
		return fail(fmt.Errorf("%s answered %d, expected %d%s", p.URL, resp.StatusCode, expect, hint))
	}
	res.OK = true
	res.Latency = time.Since(start)
	return res
}

// ---- DNS ----------------------------------------------------------------

// Resolver is the one method of *net.Resolver that DNSProber needs, so tests
// can substitute a fake.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// DNSProber resolves a name. DNS is cheaper than HTTP and sometimes works when
// HTTP is blocked, but a cached answer can make a dead network look alive, so
// it is best combined with an HTTP probe (see Any).
type DNSProber struct {
	Host     string
	Resolver Resolver // default net.DefaultResolver
}

func (p DNSProber) Probe(ctx context.Context) Result {
	start := time.Now()
	r := p.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupHost(ctx, p.Host)
	res := Result{Target: "dns:" + p.Host, Latency: time.Since(start)}
	switch {
	case err != nil:
		res.Err = err
	case len(addrs) == 0:
		res.Err = fmt.Errorf("dns: %s resolved to nothing", p.Host)
	default:
		res.OK = true
	}
	return res
}

// ---- several at once ------------------------------------------------------

// Any probes all of its targets at the same time and succeeds as soon as one
// does. One site being down must not look like "we are offline".
func Any(probers ...Prober) Prober { return anyProber(probers) }

type anyProber []Prober

func (a anyProber) Probe(ctx context.Context) Result {
	if len(a) == 0 {
		return Result{Err: errors.New("no probe targets configured")}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops the losers as soon as there is a winner

	start := time.Now()
	out := make(chan Result, len(a)) // buffered: nobody blocks, nobody leaks
	for _, p := range a {
		go func(p Prober) { out <- p.Probe(ctx) }(p)
	}
	var errs []string
	var last Result
	for range a {
		r := <-out
		if r.OK {
			return r
		}
		last = r
		if r.Err != nil {
			errs = append(errs, r.Err.Error())
		}
	}
	last.Latency = time.Since(start)
	last.Err = errors.New(strings.Join(errs, "; "))
	return last
}
