package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/syncer"
)

// Day 40: once a group has a membership, nodes talk mutual TLS 1.3 and the shared
// key is not used at all. Before there is a membership, nothing else can tell
// who may join, so the shared-key path remains for that case (and for the
// Pi/phone tests of earlier days).

// deviceIdentity loads this device's key as an identity (nil if there is none).
func deviceIdentity() (*identity.Identity, error) {
	sg, err := deviceSigner()
	if err != nil || sg == nil {
		return nil, err
	}
	id, ok := sg.(*identity.Identity)
	if !ok {
		return nil, errors.New("the device signer is not an identity key")
	}
	return id, nil
}

func hasMembership(ctx context.Context, st *sqlstore.Store) bool {
	rep, err := st.Members(ctx)
	return err == nil && len(rep.Members) > 0
}

const hmacOffHint = "this group has a membership, so the shared-key (HMAC) path is off. Use an https:// peer: both devices need their device key (offgrid identity init), and a device that has only just enrolled needs -peer-device ADMIN_ID for its first sync"

// configureMutual makes cl a mutual-TLS client for st. The server must be the
// pinned device if peerDevice is given, otherwise a device st's membership log
// names (so a device that has no membership yet must pin).
func configureMutual(cl *syncer.Client, st *sqlstore.Store, peerDevice string) error {
	id, err := deviceIdentity()
	if err != nil {
		return err
	}
	if id == nil {
		return errors.New("mutual TLS needs this device's key and there is none. Make one with: offgrid identity init")
	}
	var trust syncer.Trust
	switch {
	case peerDevice != "":
		trust = syncer.PinDevice(peerDevice)
	case hasMembership(context.Background(), st):
		trust = syncer.PinMembers(st, time.Now)
	default:
		return errors.New("this replica knows no membership yet, so it cannot tell which server to trust. Give -peer-device ADMIN_DEVICE_ID (the enrollment printed it)")
	}
	cl.Mutual = true
	cl.HTTP = syncer.NewTLSClient(syncer.ClientTLSConfig(id, trust, time.Now))
	return nil
}

// refuseHMACInAMembership is the check that makes the shared-key path off for groups that have a membership.
func refuseHMACInAMembership(ctx context.Context, st *sqlstore.Store, peer string, stderr io.Writer) bool {
	if strings.HasPrefix(peer, "https://") || !hasMembership(ctx, st) {
		return false
	}
	fmt.Fprintln(stderr, "error:", hmacOffHint)
	return true
}

// serveMutual runs the mutual-TLS sync listener (and the optional enrollment listener) until ctx ends.
func serveMutual(ctx context.Context, st *sqlstore.Store, ln net.Listener, enrollAddr string, window time.Duration, stdout, stderr io.Writer) int {
	id, err := deviceIdentity()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if id == nil {
		fmt.Fprintln(stderr, "error: this group has a membership, so this node must serve mutual TLS, which needs its device key. Make one with: offgrid identity init")
		return 1
	}
	authz := syncer.MembershipAuthorizer(st, time.Now)
	cfg, err := syncer.ServerTLSConfig(id, authz, time.Now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	lg := log.New(stderr, "sync: ", log.LstdFlags)
	srv := syncer.NewHTTPServer(syncer.Handler(st, syncer.ServerOptions{MutualTLS: true, Authorizer: authz, Window: window, Log: lg}), cfg, syncer.DefaultTimeouts)
	servers := []*http.Server{srv}
	done := make(chan error, 2)
	// Scripts and tests read these lines to learn the ports when an address ends in :0.
	fmt.Fprintf(stdout, "offgrid sync: node %s serving on https://%s (mutual TLS 1.3, device %s)\n", st.NodeID(), ln.Addr(), id.ID())
	go func() { done <- srv.ServeTLS(ln, "", "") }()
	if enrollAddr != "" {
		eln, err := net.Listen("tcp", enrollAddr)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		ecfg, err := syncer.EnrollServerTLSConfig(id, time.Now)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		esrv := syncer.NewHTTPServer(syncer.Handler(st, syncer.ServerOptions{EnrollOnly: true, Enroller: st, Log: lg}), ecfg, syncer.DefaultTimeouts)
		servers = append(servers, esrv)
		fmt.Fprintf(stdout, "offgrid sync: enrollment on https://%s (no client certificate; a new device pins this node with its invite). Stop it when you are done enrolling.\n", eln.Addr())
		go func() { done <- esrv.ServeTLS(eln, "", "") }()
	}
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, s := range servers {
			s.Shutdown(sctx)
		}
		fmt.Fprintln(stdout, "offgrid sync: stopped")
		return 0
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
}
