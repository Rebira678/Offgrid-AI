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
	"syscall"
	"text/tabwriter"
	"time"

	"offgrid/internal/phone"
	"offgrid/internal/pki"
	"offgrid/internal/store/sqlstore"
)

const phoneHelp = `offgrid phone manages the phone PWA client, pairing codes, and enrolled sessions.

Usage:
  offgrid phone pair   [-db FILE] [-label NAME] [-ttl 15m]
        Generate a one-time 6-digit code for a phone to enroll.
  offgrid phone serve  [-db FILE] [-addr HOST:PORT] [-cert FILE -key FILE] [-http-addr HOST:PORT] [-ca FILE] [-read-only]
        Serve the phone client PWA and API over HTTPS or HTTP.
  offgrid phone list   [-db FILE]
        List all enrolled phone sessions and their last seen times.
  offgrid phone revoke [-db FILE] -token TOKEN
        Revoke an enrolled phone session by its full token or token prefix.
`

func cmdPhone(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, phoneHelp)
		return 2
	}

	switch args[0] {
	case "pair":
		return cmdPhonePair(args[1:], stdout, stderr)
	case "serve":
		return cmdPhoneServe(args[1:], stdout, stderr)
	case "list":
		return cmdPhoneList(args[1:], stdout, stderr)
	case "revoke":
		return cmdPhoneRevoke(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, phoneHelp)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown phone command %q\n\n%s", args[0], phoneHelp)
		return 2
	}
}

func cmdPhonePair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("phone pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	label := fs.String("label", "Field Phone", "label identifying this phone")
	ttl := fs.Duration("ttl", phone.DefaultPairingTTL, "how long before the pairing code expires (e.g. 15m)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	st, err := openExistingStore(ctx, *dbPath, sqlstore.Options{})
	if err != nil {
		if code, ok := noDatabaseExit(stderr, err); ok {
			return code
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		fmt.Fprintf(stderr, "error: session store init: %v\n", err)
		return 1
	}

	pairCode, err := sessions.CreatePairingCode(ctx, *label, *ttl)
	if err != nil {
		fmt.Fprintf(stderr, "error: create pairing code: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Offgrid Phone Pairing\n")
	fmt.Fprintf(stdout, "---------------------\n")
	fmt.Fprintf(stdout, "Pairing code: %s\n", pairCode.Code)
	fmt.Fprintf(stdout, "Expires:      in %s (%s UTC)\n", ttl.Round(time.Second), pairCode.ExpiresAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(stdout, "Label:        %s\n\n", pairCode.Label)
	fmt.Fprintf(stdout, "On your phone browser, open the node URL and enter this pairing code.\n")
	return 0
}

func cmdPhoneServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("phone serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	addr := fs.String("addr", ":8443", "address to serve the phone PWA (host:port)")
	certFile := fs.String("cert", "", "TLS certificate file (.crt) for HTTPS")
	keyFile := fs.String("key", "", "TLS private key file (.key) for HTTPS")
	caFile := fs.String("ca", "", "public CA certificate (.crt) to offer over HTTP")
	httpAddr := fs.String("http-addr", "", "plain HTTP address to serve ca.crt (e.g. :8080); empty disables")
	readOnly := fs.Bool("read-only", false, "refuse task modifications and resolves")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openExistingStore(ctx, *dbPath, sqlstore.Options{})
	if err != nil {
		if code, ok := noDatabaseExit(stderr, err); ok {
			return code
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		fmt.Fprintf(stderr, "error: session store init: %v\n", err)
		return 1
	}

	var tlsConfig *tls.Config
	isTLS := false
	if *certFile != "" && *keyFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			fmt.Fprintf(stderr, "error: load TLS keys: %v\n", err)
			return 1
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13,
		}
		isTLS = true
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(stderr, "error: listen on %s: %v\n", *addr, err)
		return 1
	}
	defer ln.Close()

	if isTLS {
		ln = tls.NewListener(ln, tlsConfig)
	}

	// If CA download helper requested, launch in background
	if *caFile != "" && *httpAddr != "" {
		ca, err := pki.LoadCACert(*caFile)
		if err != nil {
			fmt.Fprintf(stderr, "error: load CA certificate %s: %v\n", *caFile, err)
			return 1
		}
		httpLn, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			fmt.Fprintf(stderr, "error: listen on %s: %v\n", *httpAddr, err)
			return 1
		}
		defer httpLn.Close()

		caMux := http.NewServeMux()
		caMux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-x509-ca-cert")
			w.Header().Set("Content-Disposition", "attachment; filename=\"offgrid-ca.crt\"")
			_, _ = w.Write(ca.CertPEM)
		})
		caSrv := &http.Server{Handler: caMux}
		go func() { _ = caSrv.Serve(httpLn) }()
		fmt.Fprintf(stdout, "Serving CA certificate at http://%s/ for phones to install\n", httpLn.Addr())
	}

	scheme := "http"
	if isTLS {
		scheme = "https"
	}
	fmt.Fprintf(stdout, "Offgrid Phone PWA active at %s://%s/\n", scheme, ln.Addr())
	fmt.Fprintf(stdout, "Node ID: %s | ReadOnly: %v\n", st.NodeID(), *readOnly)
	fmt.Fprintf(stdout, "Generate pairing code with: offgrid phone pair -db %s\n", *dbPath)

	cfg := phone.Config{
		Store:         st,
		Sessions:      sessions,
		Now:           time.Now,
		SecureCookies: isTLS,
		ReadOnly:      *readOnly,
	}

	if err := phone.Serve(ctx, ln, cfg); err != nil {
		fmt.Fprintf(stderr, "server exited: %v\n", err)
		return 1
	}
	return 0
}

func cmdPhoneList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("phone list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)

	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	st, err := openExistingStore(ctx, *dbPath, sqlstore.Options{})
	if err != nil {
		if code, ok := noDatabaseExit(stderr, err); ok {
			return code
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		fmt.Fprintf(stderr, "error: session store init: %v\n", err)
		return 1
	}

	list, err := sessions.ListSessions(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "error: list sessions: %v\n", err)
		return 1
	}

	if len(list) == 0 {
		fmt.Fprintln(stdout, "No enrolled phone sessions found.")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TOKEN PREFIX\tLABEL\tENROLLED\tLAST SEEN\tSTATUS")
	for _, s := range list {
		prefix := s.Token
		if len(prefix) > 12 {
			prefix = prefix[:12] + "..."
		}
		status := "active"
		if s.Revoked {
			status = "revoked"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			prefix,
			s.Label,
			s.CreatedAt.Format("2006-01-02 15:04:05"),
			s.LastSeenAt.Format("2006-01-02 15:04:05"),
			status,
		)
	}
	tw.Flush()
	return 0
}

func cmdPhoneRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("phone revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	token := fs.String("token", "", "token or token prefix to revoke")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" {
		fmt.Fprintln(stderr, "error: -token is required")
		return 2
	}

	ctx := context.Background()
	st, err := openExistingStore(ctx, *dbPath, sqlstore.Options{})
	if err != nil {
		if code, ok := noDatabaseExit(stderr, err); ok {
			return code
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		fmt.Fprintf(stderr, "error: session store init: %v\n", err)
		return 1
	}

	n, err := sessions.RevokeSession(ctx, *token)
	if err != nil {
		fmt.Fprintf(stderr, "error: revoke session: %v\n", err)
		return 1
	}
	if n == 0 {
		fmt.Fprintf(stderr, "warning: no active sessions matched token prefix %q\n", *token)
		return 1
	}

	fmt.Fprintf(stdout, "Revoked %d phone session(s) matching %q\n", n, *token)
	return 0
}
