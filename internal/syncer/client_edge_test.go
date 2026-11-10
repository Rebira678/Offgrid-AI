package syncer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

func TestHandlerNegativeOptionsFallbackToDefaults(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")

	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := httptest.NewServer(Handler(st, ServerOptions{
		Key:      key,
		Window:   -5 * time.Minute,
		MaxBatch: -10,
	}))
	defer srv.Close()

	clientSt, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	defer clientSt.Close()

	cl := &Client{
		Peer:  srv.URL,
		Node:  "cli",
		Key:   key,
		HTTP:  srv.Client(),
		Batch: -1,
	}
	if _, err := cl.Once(ctx, clientSt); err != nil {
		t.Fatalf("sync with negative ServerOptions.Window/MaxBatch failed: %v", err)
	}
}

type countingFloodReader struct {
	remaining int
	readBytes int
}

func (c *countingFloodReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > c.remaining {
		n = c.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'A'
	}
	c.remaining -= n
	c.readBytes += n
	return n, nil
}

func TestServerAdversarialNilBodyAndUnauthenticatedFlood(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	h := Handler(st, ServerOptions{Key: key, Now: func() time.Time { return now }})

	// 1. Nil r.Body must never panic even when signed headers match empty body.
	sig := SignRequest(key, http.MethodGet, PathSummary, "cli", now.Unix(), nil)
	rNil := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: PathSummary},
		Header: http.Header{
			http.CanonicalHeaderKey(HeaderNode): {"cli"},
			http.CanonicalHeaderKey(HeaderTime): {strconv.FormatInt(now.Unix(), 10)},
			http.CanonicalHeaderKey(HeaderSig):  {sig},
		},
		Body: nil,
	}
	wNil := httptest.NewRecorder()
	h.ServeHTTP(wNil, rNil)
	if wNil.Code != http.StatusOK {
		t.Fatalf("nil Body valid signed GET returned %d: %s", wNil.Code, wNil.Body.String())
	}

	// 2. Missing auth headers or non-numeric time must be rejected BEFORE reading body bytes.
	flood := &countingFloodReader{remaining: 20 << 20}
	rUnauth := &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Path: PathPush},
		Header: http.Header{},
		Body:   io.NopCloser(flood),
	}
	wUnauth := httptest.NewRecorder()
	h.ServeHTTP(wUnauth, rUnauth)
	if wUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("missing headers returned %d, want 401", wUnauth.Code)
	}
	if flood.readBytes != 0 {
		t.Fatalf("unauthenticated missing-header request read %d body bytes, want 0", flood.readBytes)
	}

	floodBadTime := &countingFloodReader{remaining: 20 << 20}
	rBadTime := &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Path: PathPush},
		Header: http.Header{
			http.CanonicalHeaderKey(HeaderNode): {"x"},
			http.CanonicalHeaderKey(HeaderTime): {"not-a-unix-ts"},
			http.CanonicalHeaderKey(HeaderSig):  {"deadbeef"},
		},
		Body: io.NopCloser(floodBadTime),
	}
	wBadTime := httptest.NewRecorder()
	h.ServeHTTP(wBadTime, rBadTime)
	if wBadTime.Code != http.StatusUnauthorized {
		t.Fatalf("non-numeric time returned %d, want 401", wBadTime.Code)
	}
	if floodBadTime.readBytes != 0 {
		t.Fatalf("malformed timestamp request read %d body bytes, want 0", floodBadTime.readBytes)
	}

	// 3. Valid header shape with oversized body (> 16 MiB) must return 413 cleanly.
	floodHuge := &countingFloodReader{remaining: (16 << 20) + 128}
	rHuge := &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Path: PathPush},
		Header: http.Header{
			http.CanonicalHeaderKey(HeaderNode): {"x"},
			http.CanonicalHeaderKey(HeaderTime): {strconv.FormatInt(now.Unix(), 10)},
			http.CanonicalHeaderKey(HeaderSig):  {"deadbeef"},
		},
		Body: io.NopCloser(floodHuge),
	}
	wHuge := httptest.NewRecorder()
	h.ServeHTTP(wHuge, rHuge)
	if wHuge.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body returned %d, want 413", wHuge.Code)
	}
}

func TestClientRejectsTamperedResponseSignature(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")

	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hashBefore, err := st.StateHash(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Peer returns valid JSON 200 OK, but with an invalid X-Offgrid-Sig header.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte(`{"node":"evil","protocol":1,"summary":{}}`)
		w.Header().Set(HeaderSig, "0000000000000000000000000000000000000000000000000000000000000000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cl := &Client{Peer: srv.URL, Node: "cli", Key: key, HTTP: srv.Client()}
	_, err = cl.Once(ctx, st)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature on tampered peer response, got %v", err)
	}
	if hashAfter, _ := st.StateHash(ctx); hashAfter != hashBefore {
		t.Fatalf("client state mutated despite bad signature")
	}
}
