package main

import (
	"context"
	"crypto/tls"
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

	"offgrid/internal/pki"
	"offgrid/internal/spike"
)

const caHelp = `offgrid ca makes the small private certificate authority a site needs for HTTPS on its own network.

  offgrid ca init  -dir CADIR [-name NAME] [-permit CIDR|domain]... [-only CIDR|domain]...
        Make a CA in CADIR (ca.crt public, ca.key SECRET). Keep ca.key on a USB stick, off the node.
        The CA can only vouch for private addresses and the names .local, .lan, .offgrid
        (-permit adds to that, -only replaces it with something narrower).
  offgrid ca issue -dir CADIR -out NODEDIR -name HOST_OR_IP [-name ...] [-days 365]
        Sign a certificate for the node (node.crt, node.key in NODEDIR). Needs ca.key: plug the stick in.
  offgrid ca show  FILE.crt
        What a certificate says: names, dates, days left, what a CA may issue for, fingerprint.
  offgrid ca spike -cert node.crt -key node.key -ca ca.crt [-addr :8443] [-http-addr :8080] [-results FILE] [-for 1h]
        The Day 33 phone check: an HTTPS page that tests service worker, offline reload, IndexedDB and a
        device key, and a plain page that hands the phone ca.crt to install.

Why: a phone only runs offline pages and device keys on a trusted HTTPS address, a public certificate needs the
internet to renew, and a site node has none. See docs/day-33.md.`

func cmdCA(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stderr, caHelp)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "init":
		return cmdCAInit(args[1:], stdout, stderr)
	case "issue":
		return cmdCAIssue(args[1:], stdout, stderr)
	case "show":
		return cmdCAShow(args[1:], stdout, stderr)
	case "spike":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return cmdCASpike(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "error: unknown ca command %q (init, issue, show or spike)\n", args[0])
	return 2
}

// repeated is a flag that may be given several times.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func cmdCAInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ca init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "folder for the new CA (created, mode 0700)")
	name := fs.String("name", "Offgrid site CA", "name shown in the phone's certificate list")
	years := fs.Int("years", pki.DefaultCAYears, "how long the CA is valid")
	var permit, only repeated
	fs.Var(&permit, "permit", "an extra CIDR or domain suffix the CA may issue for (repeatable)")
	fs.Var(&only, "only", "REPLACE the allowed names and CIDRs with these (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "error: -dir CADIR is required (and nothing else positional)")
		return 2
	}
	ca, err := pki.InitCA(*dir, pki.CAOptions{Name: *name, Years: *years, Permit: permit, Only: only})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, pki.Describe(ca.Cert, time.Time{}))
	fmt.Fprintf(stdout, "\nwrote %s (public: give it to phones) and %s (SECRET)\n", filepath.Join(*dir, pki.CACertFile), filepath.Join(*dir, pki.CAKeyFile))
	fmt.Fprintf(stdout, "next: copy %s to a USB stick and delete it from this machine once you have issued the node certificate.\n", pki.CAKeyFile)
	fmt.Fprintln(stdout, "      Whoever holds that file can make certificates your phones trust (only for the names above).")
	return 0
}

func cmdCAIssue(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ca issue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "the CA folder (needs ca.key)")
	out := fs.String("out", "", "folder for node.crt and node.key (created, mode 0700)")
	days := fs.Int("days", pki.DefaultLeafDays, "validity in days (at most 825: iPhones refuse longer)")
	var names repeated
	fs.Var(&names, "name", "a host name or IP address the node is reached by (repeatable; use the IP the phone types)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || *out == "" || len(names) == 0 || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "error: -dir CADIR, -out NODEDIR and at least one -name are required")
		return 2
	}
	ca, err := pki.LoadCA(*dir, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	leaf, err := ca.Issue(pki.LeafOptions{Names: names, Days: *days})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := leaf.Write(*out); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, pki.Describe(leaf.Cert, time.Time{}))
	fmt.Fprintf(stdout, "\nwrote %s and %s\n", filepath.Join(*out, "node.crt"), filepath.Join(*out, "node.key"))
	fmt.Fprintln(stdout, "the node needs these two files and ca.crt; it does NOT need ca.key.")
	return 0
}

func cmdCAShow(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: offgrid ca show FILE.crt")
		return 2
	}
	c, err := pki.ParseCertFile(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, pki.Describe(c, time.Time{}))
	return 0
}

func cmdCASpike(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ca spike", flag.ContinueOnError)
	fs.SetOutput(stderr)
	certPath := fs.String("cert", "", "node.crt")
	keyPath := fs.String("key", "", "node.key")
	caPath := fs.String("ca", "", "ca.crt (the public CA certificate; NOT ca.key)")
	addr := fs.String("addr", ":8443", "HTTPS address")
	httpAddr := fs.String("http-addr", ":8080", "plain HTTP address that hands out ca.crt; empty turns it off")
	results := fs.String("results", "spike-results.jsonl", "file that receives each phone's result (JSON lines)")
	runFor := fs.Duration("for", time.Hour, "stop after this long: a spike is not a service")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *certPath == "" || *keyPath == "" || *caPath == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "error: -cert, -key and -ca are required")
		return 2
	}
	leaf, key, err := pki.LoadKeyPair(*certPath, *keyPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	ca, err := pki.LoadCACert(*caPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(*caPath), pki.CAKeyFile)); err == nil {
		fmt.Fprintf(stderr, "warning: %s is on this machine. Move it to a USB stick: the node does not need it, and anyone who gets it can sign certificates your phones trust.\n", filepath.Join(filepath.Dir(*caPath), pki.CAKeyFile))
	}
	if time.Now().After(leaf.NotAfter) {
		fmt.Fprintf(stderr, "error: the node certificate expired on %s; issue a new one with: offgrid ca issue\n", leaf.NotAfter.Format("2006-01-02"))
		return 1
	}
	if miss := namesNotOnThisMachine(leaf.IPAddresses); miss != "" {
		fmt.Fprintln(stderr, miss)
	}
	srv, err := spike.New(spike.Options{CA: ca.Cert, CAPEM: ca.CertPEM, ResultsPath: *results, Log: stdout})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}},
	}
	httpsLn, err := tls.Listen("tcp", *addr, tlsCfg)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	s1 := &http.Server{Handler: srv.HTTPS(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	servers := []*http.Server{s1}
	go func() { _ = s1.Serve(httpsLn) }()
	var httpPort string
	if *httpAddr != "" {
		ln, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			_ = s1.Close()
			return 1
		}
		s2 := &http.Server{Handler: srv.HTTP(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
		servers = append(servers, s2)
		go func() { _ = s2.Serve(ln) }()
		_, httpPort, _ = net.SplitHostPort(ln.Addr().String())
	}
	_, httpsPort, _ := net.SplitHostPort(httpsLn.Addr().String())

	fmt.Fprintf(stdout, "phone check is up for %s (Ctrl-C to stop)\n\n", *runFor)
	fmt.Fprintf(stdout, "CA fingerprint (SHA-256): %s\n\n", pki.Fingerprint(ca.Cert))
	for _, ip := range lanIPs() {
		if httpPort != "" {
			fmt.Fprintf(stdout, "  1. on the phone, install the CA:  http://%s/\n", net.JoinHostPort(ip, httpPort))
		}
		fmt.Fprintf(stdout, "  2. then open the check:           https://%s/\n", net.JoinHostPort(ip, httpsPort))
	}
	fmt.Fprintf(stdout, "\nresults are written to %s as they arrive\n\n", *results)

	ctx, cancel := context.WithTimeout(ctx, *runFor)
	defer cancel()
	<-ctx.Done()
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	fmt.Fprintln(stdout, "stopped")
	return 0
}

// lanIPs lists this machine's private, non-loopback addresses: what a phone on the same network would use.
func lanIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil && n.IP.IsPrivate() {
			out = append(out, n.IP.String())
		}
	}
	return out
}

// namesNotOnThisMachine warns when none of the certificate's IP addresses belong to this
// machine: a phone that types this machine's address would get a name-mismatch error, the most
// common way this test fails, and the one that looks like a broken CA.
func namesNotOnThisMachine(certIPs []net.IP) string {
	if len(certIPs) == 0 {
		return ""
	}
	mine := lanIPs()
	for _, c := range certIPs {
		for _, m := range mine {
			if c.String() == m {
				return ""
			}
		}
	}
	if len(mine) == 0 {
		return ""
	}
	return fmt.Sprintf("warning: the node certificate is for %v, but this machine's addresses are %s. A phone that opens this machine's address will see a name-mismatch warning. Issue again with -name <that address>.", certIPs, strings.Join(mine, ", "))
}
