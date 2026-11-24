package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/blobstore"
	"offgrid/internal/store/sqlstore"
)

func jpegBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31+int(seed)) ^ byte(i>>9)
	}
	copy(b, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0})
	return b
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func photo(t testing.TB, s *sqlstore.Store, taskID string, data []byte) sqlstore.Attachment {
	t.Helper()
	a, err := s.AddAttachment(context.Background(), taskID, bytes.NewReader(data), int64(len(data)), "image/jpeg", "p.jpg")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func present(t testing.TB, s *sqlstore.Store, hash string) bool {
	t.Helper()
	bs, err := s.Blobs()
	if err != nil {
		t.Fatal(err)
	}
	return bs.Has(hash)
}

// blobEnv is two replicas, a serving one wrapped so tests can count and cut blob requests.
type blobEnv struct {
	clk      *fakeClock
	srvStore *sqlstore.Store
	srv      *httptest.Server
	hits     struct{ check, get, put atomic.Int64 }
	served   atomic.Int64 // blob bytes the server wrote
	inflight atomic.Int64
	maxInfl  atomic.Int64
	slow     time.Duration
	cutGet   func(n int64) int64 // for the n-th GET, the byte count after which to cut (0: don't)
	getN     atomic.Int64
	ranges   []string
	mu       sync.Mutex
}

func newBlobEnv(t testing.TB) *blobEnv {
	t.Helper()
	e := &blobEnv{clk: &fakeClock{t0}}
	e.srvStore = newStore(t, "node-srv", e.clk)
	inner := Handler(e.srvStore, ServerOptions{Key: key, Now: e.clk.now})
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == PathBlobCheck:
			e.hits.check.Add(1)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, PathBlob):
			e.hits.get.Add(1)
			n := e.inflight.Add(1)
			defer e.inflight.Add(-1)
			for {
				m := e.maxInfl.Load()
				if n <= m || e.maxInfl.CompareAndSwap(m, n) {
					break
				}
			}
			e.mu.Lock()
			e.ranges = append(e.ranges, r.Header.Get("Range"))
			e.mu.Unlock()
			if e.slow > 0 {
				time.Sleep(e.slow)
			}
			cw := &cutWriter{ResponseWriter: w, env: e}
			if e.cutGet != nil {
				cw.limit = e.cutGet(e.getN.Add(1))
			}
			w = cw
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, PathBlob):
			e.hits.put.Add(1)
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// cutWriter aborts the connection after limit bytes of a response (limit 0: never).
type cutWriter struct {
	http.ResponseWriter
	env     *blobEnv
	limit   int64
	written int64
}

func (c *cutWriter) Write(p []byte) (int, error) {
	if c.limit > 0 && c.written+int64(len(p)) > c.limit {
		keep := c.limit - c.written
		c.ResponseWriter.Write(p[:keep])
		c.env.served.Add(keep)
		c.ResponseWriter.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	n, err := c.ResponseWriter.Write(p)
	c.written += int64(n)
	c.env.served.Add(int64(n))
	return n, err
}

func (e *blobEnv) client(t testing.TB, local *sqlstore.Store) *Client {
	return &Client{Peer: e.srv.URL, Key: key, Node: local.NodeID(), Now: e.clk.now, Blob: BlobOptions{Backoff: -1}}
}

func TestAPhotoAddedOnOneDeviceAppearsOnTheOtherWithTheSameHash(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	data := jpegBytes(600_000, 1)
	att := photo(t, e.srvStore, tk.ID, data)

	cl := e.client(t, b)
	rep, err := cl.Once(ctx, b)
	if err != nil || rep.BlobErr != nil {
		t.Fatalf("%v %v", err, rep.BlobErr)
	}
	if rep.Blobs.Pulled != 1 || rep.Blobs.PulledBytes != int64(len(data)) {
		t.Fatalf("%+v", rep.Blobs)
	}
	got, _ := b.Attachments(ctx, tk.ID)
	if len(got) != 1 || got[0].Hash != att.Hash || !got[0].Present {
		t.Fatalf("%+v", got)
	}
	f, _, err := b.OpenAttachment(ctx, tk.ID, att.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	back, _ := io.ReadAll(f)
	if sha(back) != att.Hash {
		t.Fatal("the bytes on b are not the bytes of a")
	}
	if hash(t, b) != hash(t, e.srvStore) {
		t.Fatal("state differs")
	}
}

func TestAPhotoAddedOnTheCallerIsPushedToThePeer(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, b, "pump")
	data := jpegBytes(400_000, 2)
	att := photo(t, b, tk.ID, data)
	rep, err := e.client(t, b).Once(ctx, b)
	if err != nil || rep.BlobErr != nil {
		t.Fatalf("%v %v", err, rep.BlobErr)
	}
	if rep.Blobs.Pushed != 1 || !present(t, e.srvStore, att.Hash) {
		t.Fatalf("%+v", rep.Blobs)
	}
	if err := mustBlobs(t, e.srvStore).Verify(att.Hash); err != nil {
		t.Fatal(err)
	}
}

func mustBlobs(t testing.TB, s *sqlstore.Store) *blobstore.Store {
	t.Helper()
	bs, err := s.Blobs()
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

func TestABlobThePeerAlreadyHasIsNeverSentAndNothingIsRequestedTwice(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	photo(t, e.srvStore, tk.ID, jpegBytes(300_000, 3))
	cl := e.client(t, b)
	if _, err := cl.Once(ctx, b); err != nil {
		t.Fatal(err)
	}
	// both sides hold it now: another round moves nothing
	getsBefore, putsBefore := e.hits.get.Load(), e.hits.put.Load()
	servedBefore := e.served.Load()
	rep, err := cl.Once(ctx, b)
	if err != nil || rep.BlobErr != nil {
		t.Fatalf("%v %v", err, rep.BlobErr)
	}
	if e.hits.get.Load() != getsBefore || e.hits.put.Load() != putsBefore || e.served.Load() != servedBefore {
		t.Fatalf("a second round moved blobs: gets %d->%d puts %d->%d", getsBefore, e.hits.get.Load(), putsBefore, e.hits.put.Load())
	}
	if rep.Blobs.Pulled+rep.Blobs.Pushed != 0 {
		t.Fatalf("%+v", rep.Blobs)
	}
}

func TestACorruptedBlobIsRejectedAndNothingIsKept(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	data := jpegBytes(200_000, 4)
	att := photo(t, e.srvStore, tk.ID, data)
	// bit rot on the serving side: same length, one byte different
	p := filepath.Join(mustBlobs(t, e.srvStore).Dir(), att.Hash[:2], att.Hash[2:])
	bad := append([]byte(nil), data...)
	bad[100_000] ^= 0x55
	if err := os.WriteFile(p, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := e.client(t, b).Once(ctx, b)
	if err != nil || rep.BlobErr != nil {
		t.Fatalf("%v %v", err, rep.BlobErr)
	}
	if rep.Blobs.Corrupt != 1 || rep.Blobs.Pulled != 0 {
		t.Fatalf("%+v", rep.Blobs)
	}
	if present(t, b, att.Hash) || mustBlobs(t, b).PartialSize(att.Hash) != 0 {
		t.Fatal("a corrupted blob (or its partial) was kept")
	}
	// the attachment is still listed, honestly marked as not here
	got, _ := b.Attachments(ctx, tk.ID)
	if len(got) != 1 || got[0].Present {
		t.Fatalf("%+v", got)
	}
}

func TestAPullCutHalfwayResumesWithARangeRequestAndDoesNotResendWhatArrived(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	data := jpegBytes(900_000, 5)
	att := photo(t, e.srvStore, tk.ID, data)
	e.cutGet = func(n int64) int64 {
		if n == 1 {
			return 350_000 // the first GET dies after 350,000 bytes
		}
		return 0
	}
	rep, err := e.client(t, b).Once(ctx, b)
	if err != nil || rep.BlobErr != nil {
		t.Fatalf("%v %v", err, rep.BlobErr)
	}
	if rep.Blobs.Pulled != 1 || rep.Blobs.Resumed != 1 || rep.Blobs.Retried != 1 {
		t.Fatalf("%+v", rep.Blobs)
	}
	if err := mustBlobs(t, b).Verify(att.Hash); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	ranges := append([]string(nil), e.ranges...)
	e.mu.Unlock()
	if len(ranges) != 2 || ranges[0] != "" || !strings.HasPrefix(ranges[1], "bytes=") || ranges[1] == "bytes=0-" {
		t.Fatalf("ranges asked: %q", ranges)
	}
	if total := e.served.Load(); total > int64(len(data))+8192 { // the whole blob once, plus what a cut loses in buffers
		t.Fatalf("the server wrote %d bytes for a %d byte blob: the transfer started over", total, len(data))
	}
}

func TestAPullWhoseFirstAttemptsAllFailKeepsItsPartialForTheNextRound(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	data := jpegBytes(500_000, 6)
	att := photo(t, e.srvStore, tk.ID, data)
	e.cutGet = func(n int64) int64 { return 100_000 + n } // every attempt dies early (at slightly different points)
	cl := e.client(t, b)
	cl.Blob.Retries = 1
	rep, _ := cl.Once(ctx, b)
	if rep.BlobErr == nil {
		t.Fatal("a transfer that never finished was reported as fine")
	}
	if present(t, b, att.Hash) {
		t.Fatal("half a blob is visible")
	}
	partial := mustBlobs(t, b).PartialSize(att.Hash)
	if partial < 100_000 {
		t.Fatalf("partial = %d: nothing was kept for a resume", partial)
	}
	// the link recovers: the next round continues from the partial
	e.cutGet = nil
	e.served.Store(0)
	rep, err := cl.Once(ctx, b)
	if err != nil || rep.BlobErr != nil || rep.Blobs.Resumed != 1 || !present(t, b, att.Hash) {
		t.Fatalf("%v %v %+v", err, rep.BlobErr, rep.Blobs)
	}
	if e.served.Load() > int64(len(data))-partial+8192 {
		t.Fatalf("wrote %d bytes, only %d were missing", e.served.Load(), int64(len(data))-partial)
	}
}

// cutBody makes a request body fail after n bytes, as a dropped connection would.
type cutTransport struct {
	rt   http.RoundTripper
	cuts atomic.Int64
	n    int64
}

func (c *cutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut && c.cuts.Add(1) == 1 {
		r = r.Clone(r.Context())
		r.Body = &failAfter{r: r.Body, left: c.n}
	}
	return c.rt.RoundTrip(r)
}

type failAfter struct {
	r    io.ReadCloser
	left int64
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, errors.New("connection reset by peer")
	}
	if int64(len(p)) > f.left {
		p = p[:f.left]
	}
	n, err := f.r.Read(p)
	f.left -= int64(n)
	return n, err
}
func (f *failAfter) Close() error { return f.r.Close() }

func TestAPushCutHalfwayResumesFromWhatThePeerHolds(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, b, "pump")
	data := jpegBytes(800_000, 7)
	att := photo(t, b, tk.ID, data)
	cl := e.client(t, b)
	cl.HTTP = &http.Client{Transport: &cutTransport{rt: http.DefaultTransport, n: 300_000}}
	cl.Blob.Backoff = 150 * time.Millisecond // as in real use: the server finishes with the cut upload before the retry arrives
	rep, err := cl.Once(ctx, b)
	if err != nil || rep.BlobErr != nil {
		t.Fatalf("%v %v", err, rep.BlobErr)
	}
	if rep.Blobs.Pushed != 1 || rep.Blobs.Resumed != 1 || rep.Blobs.Retried < 1 {
		t.Fatalf("%+v", rep.Blobs)
	}
	if rep.Blobs.PushedBytes >= int64(len(data)) {
		t.Fatalf("the final attempt sent %d bytes of %d: it started over", rep.Blobs.PushedBytes, len(data))
	}
	if err := mustBlobs(t, e.srvStore).Verify(att.Hash); err != nil {
		t.Fatal(err)
	}
}

func TestAnUploadOfABlobNoOpNamesIsRefused(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	data := jpegBytes(5000, 8)
	att := photo(t, e.srvStore, tk.ID, data)
	cl := e.client(t, b)
	put := func(hash string, size int, body []byte) (int, string) {
		resp, err := cl.doRaw(ctx, http.MethodPut, PathBlob+hash, url.Values{"size": {fmt.Sprint(size)}}, map[string]string{HeaderOffset: "0"}, bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		re, _ := blobRemoteError(resp)
		return resp.StatusCode, re.Code
	}
	other := []byte("free storage for everyone, please")
	if st, code := put(sha(other), len(other), other); st != http.StatusForbidden || code != CodeNotWanted {
		t.Fatalf("an unreferenced blob: %d %s", st, code)
	}
	if st, code := put(att.Hash, len(data)+1, data); st != http.StatusForbidden || code != CodeNotWanted {
		t.Fatalf("a wrong size: %d %s", st, code)
	}
	if mustBlobs(t, e.srvStore).Has(sha(other)) {
		t.Fatal("stored")
	}
	// and a request with no valid signature is refused outright
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+PathBlob+att.Hash, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unsigned GET got %d", resp.StatusCode)
	}
}

func TestPushedBytesThatDoNotMatchTheHashAreRefusedAndTheirPartialDiscarded(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	data := jpegBytes(20_000, 9)
	att := photo(t, e.srvStore, tk.ID, data)
	// make the server forget its copy so it wants the blob again
	if err := mustBlobs(t, e.srvStore).Remove(att.Hash); err != nil {
		t.Fatal(err)
	}
	cl := e.client(t, b)
	evil := append([]byte(nil), data...)
	evil[1000] ^= 1
	resp, err := cl.doRaw(ctx, http.MethodPut, PathBlob+att.Hash, url.Values{"size": {fmt.Sprint(len(data))}}, map[string]string{HeaderOffset: "0"}, bytes.NewReader(evil), int64(len(evil)))
	if err != nil {
		t.Fatal(err)
	}
	re, _ := blobRemoteError(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || re.Code != CodeBadBlob {
		t.Fatalf("%d %s", resp.StatusCode, re.Code)
	}
	if mustBlobs(t, e.srvStore).Has(att.Hash) || mustBlobs(t, e.srvStore).PartialSize(att.Hash) != 0 {
		t.Fatal("kept corrupt bytes")
	}
}

func TestTransfersRunAFewAtATimeAndNoMore(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	e.slow = 60 * time.Millisecond
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	for i := 0; i < 8; i++ {
		photo(t, e.srvStore, tk.ID, jpegBytes(20_000+i, byte(i)))
	}
	cl := e.client(t, b)
	cl.Blob.Parallel = 3
	rep, err := cl.Once(ctx, b)
	if err != nil || rep.BlobErr != nil || rep.Blobs.Pulled != 8 {
		t.Fatalf("%v %v %+v", err, rep.BlobErr, rep.Blobs)
	}
	if m := e.maxInfl.Load(); m > 3 || m < 2 {
		t.Fatalf("up to %d transfers ran at once with a cap of 3", m)
	}
}

func TestAFullDiskHereStopsPullingAndSaysWhy(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	att := photo(t, e.srvStore, tk.ID, jpegBytes(100_000, 1))
	photo(t, e.srvStore, tk.ID, jpegBytes(100_000, 2))
	bs := mustBlobs(t, b)
	bs.SetReserve(1 << 20)
	bs.SetFreeFunc(func(string) (uint64, error) { return 1<<20 + 50_000, nil }) // not enough for a 100,000 byte file
	rep, err := e.client(t, b).Once(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blobs.Pulled != 0 || len(rep.Blobs.Refused) == 0 || !strings.Contains(strings.Join(rep.Blobs.Refused, " "), "full") || !strings.Contains(strings.Join(rep.Blobs.Refused, " "), "reserve") {
		t.Fatalf("%+v", rep.Blobs)
	}
	if present(t, b, att.Hash) || bs.PartialSize(att.Hash) != 0 {
		t.Fatal("bytes were written to a full disk")
	}
	if e.hits.get.Load() != 0 {
		t.Fatalf("%d downloads were started for a disk that could not hold them", e.hits.get.Load())
	}
	// room appears: the next round fetches both
	bs.SetFreeFunc(func(string) (uint64, error) { return 100 << 20, nil })
	rep, _ = e.client(t, b).Once(ctx, b)
	if rep.Blobs.Pulled != 2 {
		t.Fatalf("%+v", rep.Blobs)
	}
}

func TestAFullDiskOnThePeerIsReportedAndNothingIsSent(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, b, "pump")
	photo(t, b, tk.ID, jpegBytes(100_000, 3))
	sbs := mustBlobs(t, e.srvStore)
	sbs.SetReserve(1 << 20)
	sbs.SetFreeFunc(func(string) (uint64, error) { return 1 << 20, nil })
	rep, err := e.client(t, b).Once(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blobs.Pushed != 0 || len(rep.Blobs.Refused) != 1 || !strings.Contains(rep.Blobs.Refused[0], "peer") {
		t.Fatalf("%+v", rep.Blobs)
	}
	if e.hits.put.Load() != 0 {
		t.Fatal("a blob was uploaded to a peer that said it was full")
	}
}

// onlyReplica hides everything but the Replica methods, like a peer that predates attachments.
type onlyReplica struct{ Replica }

func TestAPeerWithoutBlobSupportIsNotAnError(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t0}
	srv := newStore(t, "node-srv", clk)
	b := newStore(t, "node-b", clk)
	tk := add(t, b, "pump")
	photo(t, b, tk.ID, jpegBytes(5000, 1))
	cl, _ := serve(t, onlyReplica{srv}, clk, key)
	cl.Node = "node-b"
	rep, err := cl.Once(ctx, b)
	if err != nil || rep.BlobErr != nil || !rep.Blobs.Unsupported {
		t.Fatalf("%v %v %+v", err, rep.BlobErr, rep.Blobs)
	}
	if hash(t, b) != hash(t, srv) {
		t.Fatal("the ops did not sync")
	}
}

func TestSyncWithoutAttachmentsMakesNoBlobRequests(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	add(t, e.srvStore, "plain")
	add(t, b, "plain too")
	rep, err := e.client(t, b).Once(ctx, b)
	if err != nil || rep.Blobs.Requests != 0 || e.hits.check.Load() != 0 {
		t.Fatalf("%v %+v checks=%d", err, rep.Blobs, e.hits.check.Load())
	}
}

func TestBlobsMoveOverMutualTLSToo(t *testing.T) {
	ctx := context.Background()
	e := newTLSEnv(t)
	tk := add(t, e.admin, "pump")
	data := jpegBytes(700_000, 3)
	att := photo(t, e.admin, tk.ID, data)
	cl := e.client(e.idb, e.ida.ID(), "pi")
	rep, err := cl.Once(ctx, e.b)
	if err != nil || rep.BlobErr != nil || rep.Blobs.Pulled != 1 {
		t.Fatalf("%v %v %+v", err, rep.BlobErr, rep.Blobs)
	}
	if err := mustBlobs(t, e.b).Verify(att.Hash); err != nil {
		t.Fatal(err)
	}
	// the other way, from a photo added on the device
	tk2 := add(t, e.b, "filter")
	att2 := photo(t, e.b, tk2.ID, jpegBytes(300_000, 4))
	rep, err = cl.Once(ctx, e.b)
	if err != nil || rep.BlobErr != nil || rep.Blobs.Pushed != 1 {
		t.Fatalf("%v %v %+v", err, rep.BlobErr, rep.Blobs)
	}
	if !present(t, e.admin, att2.Hash) {
		t.Fatal("not pushed")
	}
}

func TestAnAnonymousClientCannotFetchABlobOverMutualTLS(t *testing.T) {
	e := newTLSEnv(t)
	tk := add(t, e.admin, "pump")
	att := photo(t, e.admin, tk.ID, jpegBytes(5000, 5))
	code, _ := rawSecure(t, e, func(r *http.Request) {
		r.Method = http.MethodGet
		r.URL.Path = PathBlob + att.Hash
	})
	if code == http.StatusOK {
		t.Fatalf("got %d", code)
	}
}

func TestALargeBlobCrossesTheWireWithFlatMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 48 MiB twice")
	}
	ctx := context.Background()
	clk := &fakeClock{t0}
	mk := func(node string) *sqlstore.Store {
		s, err := sqlstore.Open(ctx, filepath.Join(t.TempDir(), "x.db"), sqlstore.Options{NodeID: node, Now: clk.now, MaxBlobBytes: 64 << 20})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	a, b := mk("node-a"), mk("node-b")
	tk := add(t, a, "big")
	const size = 48 << 20
	r := io.LimitReader(&jpegStream{}, size)
	att, err := a.AddAttachment(ctx, tk.ID, r, size, "image/jpeg", "")
	if err != nil {
		t.Fatal(err)
	}
	cl, _ := serve(t, a, clk, key)
	cl.Node = "node-b"
	cl.Blob.Backoff = -1

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	done := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-done:
				return
			case <-time.After(10 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.Load() {
					peak.Store(m.HeapAlloc)
				}
			}
		}
	}()
	rep, err := cl.Once(ctx, b)
	close(done)
	if err != nil || rep.BlobErr != nil || rep.Blobs.Pulled != 1 {
		t.Fatalf("%v %v %+v", err, rep.BlobErr, rep.Blobs)
	}
	if err := mustBlobs(t, b).Verify(att.Hash); err != nil {
		t.Fatal(err)
	}
	if g := int64(peak.Load()) - int64(before.HeapAlloc); g > 24<<20 {
		t.Fatalf("heap grew %d MiB while a %d MiB blob crossed the wire", g>>20, size>>20)
	}
}

type jpegStream struct{ pos int64 }

func (j *jpegStream) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte((j.pos + int64(i)) * 7 >> 2)
	}
	if j.pos == 0 {
		copy(p, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0})
	}
	j.pos += int64(len(p))
	return len(p), nil
}
