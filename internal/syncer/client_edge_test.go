package syncer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

// TestClientCall_NonJSONErrorBodyGracefulHandling verifies that if a reverse proxy
// or server returns an error status (e.g. 502/503) with HTML or plain text (not JSON),
// client.call gracefully parses it into a RemoteError without panicking.
func TestClientCall_NonJSONErrorBodyGracefulHandling(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	htmlBody := "<html><body>502 Bad Gateway: nginx</body></html>"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderSig, SignResponse(key, r.Header.Get(HeaderSig), http.StatusBadGateway, []byte(htmlBody)))
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(htmlBody))
	}))
	defer srv.Close()

	client := &Client{
		Peer:  srv.URL,
		Node:  "laptop",
		Key:   key,
		Now:   time.Now,
		Batch: 10,
	}

	var resp SummaryResponse
	err := client.call(context.Background(), http.MethodGet, PathSummary, nil, &resp)
	if err == nil {
		t.Fatal("expected error from 502 Bad Gateway, got nil")
	}

	var re *RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("expected *RemoteError, got: %T (%v)", err, err)
	}
	if re.Status != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", re.Status, http.StatusBadGateway)
	}
}

// TestClientSync_NilPeerSummaryPushSafety verifies that if a peer returns a nil Summary
// map in SummaryResponse, Sync does not panic with a nil map assignment during push.
func TestClientSync_NilPeerSummaryPushSafety(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")

	// Server that returns nil Summary map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case PathSummary:
			body := []byte(`{"node":"phone","protocol":1,"summary":null}`)
			w.Header().Set(HeaderSig, SignResponse(key, r.Header.Get(HeaderSig), http.StatusOK, body))
			w.WriteHeader(http.StatusOK)
			w.Write(body)
		case PathOps:
			body := []byte(`{"ops":[],"more":false}`)
			w.Header().Set(HeaderSig, SignResponse(key, r.Header.Get(HeaderSig), http.StatusOK, body))
			w.WriteHeader(http.StatusOK)
			w.Write(body)
		case PathPush:
			body := []byte(`{"applied":0,"duplicates":0,"rebuilt":false}`)
			w.Header().Set(HeaderSig, SignResponse(key, r.Header.Get(HeaderSig), http.StatusOK, body))
			w.WriteHeader(http.StatusOK)
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dbDir := t.TempDir()
	st, err := sqlstore.Open(ctx, dbDir+"/test.db", sqlstore.Options{NodeID: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	client := &Client{
		Peer:  srv.URL,
		Node:  "laptop",
		Key:   key,
		Now:   time.Now,
		Batch: 10,
	}

	rep, err := client.Once(ctx, st)
	if err != nil {
		t.Fatalf("Once with nil peer summary failed: %v", err)
	}
	if rep.Pulled != 0 || rep.Pushed != 0 {
		t.Errorf("unexpected sync report: %+v", rep)
	}
}

func TestClientConcurrentAnonymousNodeIsRaceFree(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")

	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "peer"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := httptest.NewServer(Handler(st, ServerOptions{Key: key}))
	defer srv.Close()

	client := &Client{
		Peer: srv.URL,
		Key:  key,
		HTTP: srv.Client(),
	}

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			_, err := client.PeerSummary(ctx)
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent PeerSummary failed: %v", err)
	}
	if client.Node != "" {
		t.Errorf("concurrent calls mutated Client.Node to %q", client.Node)
	}
}
