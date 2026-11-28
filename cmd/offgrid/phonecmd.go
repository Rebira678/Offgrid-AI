package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"offgrid/internal/phone"
	"offgrid/internal/pki"
	"offgrid/internal/store/sqlstore"
)

const phoneHelp = `offgrid phone: the phone client (Day 52).

  offgrid phone pair  -db FILE [-label NAME] [-ttl 15m]
  offgrid phone serve -db FILE -cert node.crt -key node.key [-addr :8443] [-host NAME ...]
                      [-secret FILE] [-late-after 2m] [-session-days 30]
  offgrid phone reset-sessions -db FILE [-secret FILE]

pair      makes a one-time pairing code (an invite for a member device). Type it into the app on the phone.
serve     serves the app over HTTPS to the phones enrolled in this node. This node must be an admin
          (it enrolls the phones). Without -cert and -key it serves plain HTTP on this machine only,
          to try the app in a desktop browser.
reset-sessions
          signs every phone out. Phones sign in again with their device keys, no new code.

A lost phone: offgrid store -db FILE revoke -device DEVICE. It is cut off within seconds, whatever its
session says. Phone pages are separate from "offgrid web", the localhost dashboard.`

func cmdPhone(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, phoneHelp)
		return 0
	}
	switch args[0] {
	case "pair":
		return phonePair(args[1:], stdout, stderr)
	case "serve":
		return phoneServe(args[1:], stdout, stderr)
	case "reset-sessions":
		return phoneReset(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown phone command %q\n\n%s\n", args[0], phoneHelp)
	return 2
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func secretPath(flagVal, db string) string {
	if flagVal != "" {
		return flagVal
	}
	return filepath.Join(filepath.Dir(db), "phone-session.key")
}

func phonePair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("phone pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	label := fs.String("label", "phone", "a name for this phone, shown in the membership list and on late changes")
	ttl := fs.Duration("ttl", 15*time.Minute, "how long the code works (at most 24h)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *db == "" {
		fmt.Fprintln(stderr, "error: use -db FILE or set OFFGRID_DB")
		return 2
	}
	ctx := context.Background()
	st, err := openExistingStore(ctx, *db, sqlstore.Options{})
	if err != nil {
		if code, ok := noDatabaseExit(stderr, err); ok {
			return code
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()
	inv, err := st.CreateInvite(ctx, sqlstore.RoleMember, *label, *ttl)
	if err != nil {
		return memberError(stderr, err)
	}
	fmt.Fprintf(stdout, "pairing code for %q, valid until %s, works once\n\n  %s\n\n", inv.Label, inv.Expires.Local().Format("15:04:05 Mon 2 Jan"), groupToken(inv.Token))
	fmt.Fprintln(stdout, "On the phone: open the node's address in the browser, type the code, tap Pair.")
	fmt.Fprintln(stdout, "Whoever types it first before it expires gets a phone enrolled in your site. Give it in person, not in a group chat.")
	return 0
}

func phoneReset(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("phone reset-sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	secret := fs.String("secret", "", "the session key file (default: phone-session.key next to the database)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *db == "" && *secret == "" {
		fmt.Fprintln(stderr, "error: use -db FILE (or -secret FILE)")
		return 2
	}
	p := secretPath(*secret, *db)
	if err := phone.RotateSecret(p); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "ok: new session key in %s. Restart `offgrid phone serve`; every phone signs in again with its own key.\n", p)
	return 0
}

func phoneServe(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return phoneServeContext(ctx, args, stdout, stderr)
}

func phoneServeContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("phone serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", os.Getenv("OFFGRID_DB"), dbHelp)
	addr := fs.String("addr", ":8443", "address to listen on")
	cert := fs.String("cert", "", "the node certificate (offgrid ca issue)")
	key := fs.String("key", "", "the node's private key")
	secret := fs.String("secret", "", "the session key file (default: phone-session.key next to the database; made if missing)")
	late := fs.Duration("late-after", phone.DefaultLateAfter, "a change that waited this long on the phone is marked late")
	days := fs.Int("session-days", 30, "how long a phone stays signed in (at most 90)")
	var hosts listFlag
	fs.Var(&hosts, "host", "a name or address phones use to reach this node (repeatable; default: any private address and .local/.lan/.offgrid names)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *db == "" {
		fmt.Fprintln(stderr, "error: use -db FILE or set OFFGRID_DB")
		return 2
	}
	if (*cert == "") != (*key == "") {
		fmt.Fprintln(stderr, "error: give both -cert and -key, or neither")
		return 2
	}
	var tc *tls.Certificate
	if *cert != "" {
		leaf, k, err := pki.LoadKeyPair(*cert, *key)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if time.Now().After(leaf.NotAfter) {
			fmt.Fprintf(stderr, "error: the node certificate expired on %s; issue a new one with: offgrid ca issue\n", leaf.NotAfter.Format("2006-01-02"))
			return 1
		}
		tc = &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: k, Leaf: leaf}
	}
	sec, err := phone.LoadOrCreateSecret(secretPath(*secret, *db))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	st, err := openExistingStore(ctx, *db, sqlstore.Options{})
	if err != nil {
		if code, ok := noDatabaseExit(stderr, err); ok {
			return code
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()
	rep, err := st.Members(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(rep.Members) == 0 {
		fmt.Fprintln(stderr, "error: no membership yet, so no phone can be enrolled. Run: offgrid store -db FILE found")
		return 1
	}
	cfg := phone.Config{Store: st, Secret: sec, Hosts: hosts, LateAfter: *late, SessionTTL: time.Duration(*days) * 24 * time.Hour}
	scheme := "https"
	if tc == nil {
		scheme = "http"
	}
	err = phone.Serve(ctx, *addr, cfg, tc, func(a net.Addr) {
		fmt.Fprintf(stdout, "offgrid phone: node %s, app on %s://%s  (Ctrl-C to stop)\n", st.NodeID(), scheme, a)
		if tc == nil {
			fmt.Fprintln(stdout, "offgrid phone: no certificate, so this is for a desktop browser on this machine only; a phone needs -cert and -key")
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
