package syncer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestBlobSyncRangeBoundaryEdges(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	data := jpegBytes(50_000, 10)
	att := photo(t, e.srvStore, tk.ID, data)

	cl := e.client(t, b)

	// 1. Valid Range at offset 10,000
	resp, err := cl.doRaw(ctx, http.MethodGet, PathBlob+att.Hash, nil, map[string]string{"Range": "bytes=10000-"}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content, got %d", resp.StatusCode)
	}
	gotBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBytes) != 40_000 {
		t.Fatalf("expected 40,000 bytes, got %d", len(gotBytes))
	}
	if !bytes.Equal(gotBytes, data[10_000:]) {
		t.Fatal("range data mismatch")
	}

	// 2. Out of bounds Range: Range >= file size (50,000)
	respOOB, err := cl.doRaw(ctx, http.MethodGet, PathBlob+att.Hash, nil, map[string]string{"Range": "bytes=50000-"}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer respOOB.Body.Close()
	if respOOB.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416 Range Not Satisfiable, got %d", respOOB.StatusCode)
	}
	if cr := respOOB.Header.Get("Content-Range"); cr != "bytes */50000" {
		t.Fatalf("expected Content-Range 'bytes */50000', got %q", cr)
	}
}

func TestBlobSyncPUTOffsetConflictAndResolution(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "pump")
	data := jpegBytes(40_000, 11)
	att := photo(t, e.srvStore, tk.ID, data)

	// Make server expect this blob and create a partial file of 15,000 bytes
	if err := mustBlobs(t, e.srvStore).Remove(att.Hash); err != nil {
		t.Fatal(err)
	}
	tr, err := mustBlobs(t, e.srvStore).Begin(att.Hash, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	_ = tr.Copy(ctx, bytes.NewReader(data[:15_000]))
	tr.Close()

	cl := e.client(t, b)

	// Client erroneously attempts to push with offset 25,000
	resp, err := cl.doRaw(ctx, http.MethodPut, PathBlob+att.Hash, url.Values{"size": {fmt.Sprint(len(data))}}, map[string]string{HeaderOffset: "25000"}, bytes.NewReader(data[25_000:]), int64(len(data)-25_000))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d", resp.StatusCode)
	}
	re, serverOffset := blobRemoteError(resp)
	if re.Code != CodeOffset || serverOffset != 15_000 {
		t.Fatalf("expected code %q and offset 15000, got code=%q offset=%d", CodeOffset, re.Code, serverOffset)
	}

	// Client resumes from offset 15,000
	resp2, err := cl.doRaw(ctx, http.MethodPut, PathBlob+att.Hash, url.Values{"size": {fmt.Sprint(len(data))}}, map[string]string{HeaderOffset: "15000"}, bytes.NewReader(data[15_000:]), int64(len(data)-15_000))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusCreated && resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 201 Created or 200 OK after resume, got %d", resp2.StatusCode)
	}
	if err := mustBlobs(t, e.srvStore).Verify(att.Hash); err != nil {
		t.Fatalf("Verify on server: %v", err)
	}
}

func TestBlobSyncPageCheckPagination(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)

	// Create 55 tasks with 10 attachments each (550 total attachments) to exceed the 500-item page check limit
	const totalAttachments = 550
	const perTask = 10
	totalTasks := totalAttachments / perTask

	for tIdx := 0; tIdx < totalTasks; tIdx++ {
		tk := add(t, e.srvStore, fmt.Sprintf("task-%d", tIdx))
		for aIdx := 0; aIdx < perTask; aIdx++ {
			idx := tIdx*perTask + aIdx
			payload := []byte(fmt.Sprintf("photo-payload-item-%d", idx))
			full := append(jpegBytes(100+idx%20, byte(idx%256)), payload...)
			photo(t, e.srvStore, tk.ID, full)
		}
	}

	// Run first round: pulls ops and up to MaxCheck (500) blobs
	cl := e.client(t, b)
	rep1, err := cl.Once(ctx, b)
	if err != nil || rep1.BlobErr != nil {
		t.Fatalf("sync round 1 error: %v %v", err, rep1.BlobErr)
	}

	if rep1.Blobs.Pulled != 500 {
		t.Fatalf("round 1 pulled = %d, want 500 (MaxCheck)", rep1.Blobs.Pulled)
	}

	// Run second round: pulls the remaining 50 blobs
	rep2, err := cl.Once(ctx, b)
	if err != nil || rep2.BlobErr != nil {
		t.Fatalf("sync round 2 error: %v %v", err, rep2.BlobErr)
	}

	if rep2.Blobs.Pulled != 50 {
		t.Fatalf("round 2 pulled = %d, want 50", rep2.Blobs.Pulled)
	}

	// Check that node-b has 0 missing blobs
	missing, err := b.MissingBlobs(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("node-b still missing %d blobs after pagination check", len(missing))
	}
}

func TestBlobSyncMutualTLSReplayDefense(t *testing.T) {
	e := newTLSEnv(t)
	tk := add(t, e.admin, "pump")
	att := photo(t, e.admin, tk.ID, jpegBytes(5000, 12))

	req := func(id string, at time.Time) func(*http.Request) {
		return func(r *http.Request) {
			r.Method = http.MethodGet
			r.URL.Path = PathBlob + att.Hash
			if id != "" {
				r.Header.Set(HeaderRequest, id)
			}
			r.Header.Set(HeaderTime, fmt.Sprint(at.Unix()))
		}
	}

	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if st, _ := rawSecure(t, e, req(id, e.clk.now())); st != http.StatusOK {
		t.Fatalf("first request got %d, want 200", st)
	}
	if st, code := rawSecure(t, e, req(id, e.clk.now())); st != http.StatusConflict || code != CodeReplay {
		t.Fatalf("replayed request got %d %s, want 409 %s", st, code, CodeReplay)
	}
}

func TestBlobSyncContextTimeoutDuringTransfer(t *testing.T) {
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)
	tk := add(t, e.srvStore, "timeout-task")
	data := jpegBytes(300_000, 15)
	photo(t, e.srvStore, tk.ID, data)

	// Context with immediate timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Microsecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond) // ensure expired

	cl := e.client(t, b)
	_, _ = cl.Once(ctx, b)
	// Server and client remain operational
}

func TestBlobSyncHMACFullRound(t *testing.T) {
	ctx := context.Background()
	e := newBlobEnv(t)
	b := newStore(t, "node-b", e.clk)

	// Add photo on server
	tk := add(t, e.srvStore, "hmac-task")
	data := jpegBytes(40_000, 20)
	att := photo(t, e.srvStore, tk.ID, data)

	// Sync via standard HMAC client
	cl := e.client(t, b)
	cl.Mutual = false
	rep, err := cl.Once(ctx, b)
	if err != nil || rep.BlobErr != nil {
		t.Fatalf("HMAC blob sync failed: %v %v", err, rep.BlobErr)
	}
	if rep.Blobs.Pulled != 1 {
		t.Fatalf("expected 1 pulled blob over HMAC, got %d", rep.Blobs.Pulled)
	}
	if err := mustBlobs(t, b).Verify(att.Hash); err != nil {
		t.Fatalf("blob verification failed: %v", err)
	}
}
