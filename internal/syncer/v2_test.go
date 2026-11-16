package syncer

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rewriteBody is a transport that lets a test change what the peer answered. Gzip is switched off so the
// test sees the frame lines.
type rewriteBody struct {
	path   string
	change func(lines []string) ([]string, error) // the answer's lines in, the lines to deliver out; an error cuts the connection after them
	seen   []http.Header
}

func (rb *rewriteBody) RoundTrip(r *http.Request) (*http.Response, error) {
	rb.seen = append(rb.seen, r.Header.Clone())
	if r.URL.Path != rb.path {
		return http.DefaultTransport.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	r.Header.Del("Accept-Encoding")
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil || resp.StatusCode != 200 {
		return resp, err
	}
	all, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	lines := strings.SplitAfter(string(all), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	out, cut := rb.change(lines)
	var rd io.Reader = strings.NewReader(strings.Join(out, ""))
	if cut != nil {
		rd = io.MultiReader(rd, errReader{cut})
	}
	resp.Body = io.NopCloser(rd)
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return resp, nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func (rb *rewriteBody) changeWith(f func(lines []string) ([]string, error)) { rb.change = f }

func twoReplicas(t *testing.T, n int) (a, b interface {
	Replica
}, srv *httptest.Server, cl *Client, hashes func() (string, string)) {
	t.Helper()
	clk := &fakeClock{t0}
	sa, sb := newStore(t, "node-a", clk), newStore(t, "node-b", clk)
	for i := 0; i < n; i++ {
		add(t, sa, fmt.Sprintf("task %d", i))
	}
	cl, srv = serve(t, sa, clk, key)
	return sa, sb, srv, cl, func() (string, string) { return hash(t, sa), hash(t, sb) }
}

// An old client gets one clear answer and changes nothing; so does a peer that is too old for us, and one that
// needs a newer version than we have.
func TestAnOldClientIsToldToUpgrade(t *testing.T) {
	ctx := context.Background()
	_, b, srv, cl, _ := twoReplicas(t, 5)

	// 1. a v1 client: valid signature, no version header (what Day 40 and before sent)
	for _, path := range []string{PathSummary, PathOps} {
		method, body := "GET", ""
		if path == PathOps {
			method, body = "POST", `{"have":{},"limit":10}`
		}
		h := signed(method, path, "old", t0, body)
		h[HeaderProto] = ""
		code, resp := raw(t, srv, method, path, h, body)
		if code != http.StatusUpgradeRequired || !strings.Contains(resp, `"upgrade_required"`) || !strings.Contains(resp, "upgrade") {
			t.Fatalf("%s from a v1 client: %d %s; want 426 upgrade_required with advice", path, code, resp)
		}
	}
	// 2. the same through the real client with its version header removed: a RemoteError that is also ErrProtocol
	cl.HTTP = &http.Client{Transport: stripProto{}}
	_, err := cl.Once(ctx, b)
	var re *RemoteError
	if !errors.As(err, &re) || re.Status != http.StatusUpgradeRequired || re.Code != CodeUpgradeRequired || !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want a 426 upgrade_required RemoteError that is ErrProtocol", err)
	}
	if s, _ := b.Summary(ctx); len(s) != 0 {
		t.Fatalf("a refused client changed the replica: %v", s)
	}
}

type stripProto struct{}

func (stripProto) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Del(HeaderProto)
	return http.DefaultTransport.RoundTrip(r)
}

func TestAPeerThatIsTooOldOrTooNewIsRefusedWithAdvice(t *testing.T) {
	clk := &fakeClock{t0}
	st := newStore(t, "node-local", clk)
	for _, tc := range []struct {
		name          string
		proto, min    int
		wantInMessage string
	}{
		{"peer on v1", 1, 0, "upgrade Offgrid on the peer"},
		{"peer needs a newer client", ProtocolVersion + 1, ProtocolVersion + 1, "upgrade Offgrid on this device"},
		{"peer is newer but still takes us", ProtocolVersion + 1, MinProtocol, ""},
	} {
		srv := mockServer(t, key, func(r *http.Request) (int, any) {
			if r.URL.Path == PathSummary {
				return http.StatusOK, SummaryResponse{Node: "mock-peer", Protocol: tc.proto, MinProtocol: tc.min, Summary: map[string]int64{}}
			}
			return http.StatusOK, OpsResponse{}
		})
		cl := &Client{Peer: srv.URL, Key: key, Now: clk.now}
		_, err := cl.Once(context.Background(), st)
		if tc.wantInMessage == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, ErrProtocol) || !strings.Contains(err.Error(), tc.wantInMessage) {
			t.Errorf("%s: err = %v, want ErrProtocol saying %q", tc.name, err, tc.wantInMessage)
		}
	}
}

// A connection cut in the middle of an answer keeps what was verified and the next round resumes from there.
func TestACutConnectionResumesFromTheCursor(t *testing.T) {
	ctx := context.Background()
	a, b, _, cl, hashes := twoReplicas(t, 250)
	_ = a
	rb := &rewriteBody{path: PathOps}
	rb.changeWith(func(lines []string) ([]string, error) { return lines[:2], io.ErrUnexpectedEOF }) // two frames of 100 arrive
	cl.HTTP = &http.Client{Transport: rb}
	rep, err := cl.Once(ctx, b)
	if !errors.Is(err, ErrStreamCut) {
		t.Fatalf("a cut answer: err = %v, want ErrStreamCut", err)
	}
	if rep.Pulled != 200 {
		t.Fatalf("first round applied %d ops, want the 200 that arrived whole", rep.Pulled)
	}
	if s, _ := b.Summary(ctx); s["node-a"] != 200 {
		t.Fatalf("replica holds %v after the cut, want node-a at 200", s)
	}

	cl.HTTP = nil // the network is back
	rep, err = cl.Once(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pulled != 50 || rep.PulledDupes != 0 {
		t.Fatalf("resumed round pulled %d (+%d already held), want only the 50 it lacked", rep.Pulled, rep.PulledDupes)
	}
	if h1, h2 := hashes(); h1 != h2 {
		t.Fatal("the replicas differ after resuming")
	}
}

// An answer that stops before its end marker is a cut even if every frame it did send was fine.
func TestAnAnswerWithoutItsEndMarkerIsACut(t *testing.T) {
	ctx := context.Background()
	_, b, _, cl, _ := twoReplicas(t, 30)
	rb := &rewriteBody{path: PathOps}
	rb.changeWith(func(lines []string) ([]string, error) { return lines[:len(lines)-1], nil }) // clean EOF, marker missing
	cl.HTTP = &http.Client{Transport: rb}
	rep, err := cl.Once(ctx, b)
	if !errors.Is(err, ErrStreamCut) {
		t.Fatalf("err = %v, want ErrStreamCut", err)
	}
	if rep.Pulled != 30 {
		t.Fatalf("the verified frames were not kept: pulled %d", rep.Pulled)
	}
}

// Frames that are altered, dropped from the middle or put in another order are refused, and what they carried
// is not applied.
func TestAlteredDroppedOrReorderedFramesAreRefused(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(l []string) []string{
		"a byte changed": func(l []string) []string {
			l[0] = strings.Replace(l[0], "task 3", "task X", 1)
			return l
		},
		"a frame dropped from the middle": func(l []string) []string { return append(l[:1:1], l[2:]...) },
		"frames swapped":                  func(l []string) []string { l[0], l[1] = l[1], l[0]; return l },
		"a frame repeated":                func(l []string) []string { return append(l[:2:2], l[1:]...) },
		"the mac removed":                 func(l []string) []string { l[0] = "- " + strings.SplitN(l[0], " ", 2)[1]; return l },
	} {
		_, b, _, cl, _ := twoReplicas(t, 250)
		rb := &rewriteBody{path: PathOps}
		change := change
		rb.changeWith(func(lines []string) ([]string, error) { return change(append([]string(nil), lines...)), nil })
		cl.HTTP = &http.Client{Transport: rb}
		rep, err := cl.Once(ctx, b)
		if !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: err = %v, want ErrBadFrame", name, err)
		}
		if name == "a byte changed" || name == "the mac removed" {
			if rep.Pulled != 0 {
				t.Errorf("%s: %d ops were applied from a bad frame", name, rep.Pulled)
			}
		}
	}
}

// A frame cannot be lifted from one answer into another: the chain starts at this request's own signature.
func TestAFrameFromAnotherAnswerIsRefused(t *testing.T) {
	ctx := context.Background()
	_, b, _, cl, _ := twoReplicas(t, 20)
	var first []string
	rb := &rewriteBody{path: PathOps}
	rb.changeWith(func(lines []string) ([]string, error) {
		if first == nil {
			first = append([]string(nil), lines...)
			return lines, nil
		}
		return first, nil // replay the first answer in reply to the second request
	})
	cl.HTTP = &http.Client{Transport: rb}
	cl.Batch = 5
	if _, err := cl.Once(ctx, b); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("err = %v, want ErrBadFrame for a replayed answer", err)
	}
}

// Large answers and pushes travel gzip-compressed, and a compressed body is still bounded.
func TestCompressionAnswersAndPushes(t *testing.T) {
	ctx := context.Background()
	_, b, srv, cl, hashes := twoReplicas(t, 300)

	rec := &rewriteBody{path: "/never"}
	rec.changeWith(nil)
	cl.HTTP = &http.Client{Transport: rec}
	if _, err := cl.Once(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, h := range rec.seen {
		if h.Get(HeaderProto) != fmt.Sprint(ProtocolVersion) {
			t.Fatalf("a request without %s: %v", HeaderProto, h)
		}
	}

	// the answer is compressed when the client allows it, and smaller by a good margin than the same answer plain
	get := func(acceptGzip bool) (enc string, size int) {
		body := `{"have":{},"limit":500}`
		hdr := signed("POST", PathOps, "n", t0, body)
		req, _ := http.NewRequest("POST", srv.URL+PathOps, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		req.Header.Set(HeaderProto, fmt.Sprint(ProtocolVersion))
		if acceptGzip {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		tr := &http.Transport{DisableCompression: true}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		n, _ := io.Copy(io.Discard, resp.Body)
		return resp.Header.Get("Content-Encoding"), int(n)
	}
	encPlain, plain := get(false)
	encGz, small := get(true)
	if encPlain != "" || encGz != "gzip" {
		t.Fatalf("Content-Encoding plain=%q gzip=%q", encPlain, encGz)
	}
	if small*3 > plain {
		t.Fatalf("compressed answer is %d bytes against %d plain: expected under a third", small, plain)
	}

	// a big push is sent compressed and applied (the other direction: b pushes to a's server)
	clk := &fakeClock{t0}
	c := newStore(t, "node-c", clk)
	for i := 0; i < 200; i++ {
		add(t, c, fmt.Sprintf("pushed task number %d with a repetitive title", i))
	}
	var pushHdr http.Header
	cl2 := &Client{Peer: srv.URL, Key: key, Now: clk.now, HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == PathPush {
			pushHdr = r.Header.Clone()
		}
		return http.DefaultTransport.RoundTrip(r)
	})}}
	rep, err := cl2.Once(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pushed != 200 {
		t.Fatalf("pushed %d, want 200", rep.Pushed)
	}
	if pushHdr.Get("Content-Encoding") != "gzip" {
		t.Fatalf("a large push was not compressed: %v", pushHdr)
	}
	_ = hashes

	// a body that is not gzip, or that unpacks to more than the limit, is refused with 400 (signed, so the
	// signature check is not what stops it)
	bomb := func() []byte {
		var zb bytes.Buffer
		zw := gzip.NewWriter(&zb)
		zero := make([]byte, 1<<20)
		for i := 0; i < maxBody>>20+2; i++ {
			zw.Write(zero)
		}
		zw.Close()
		return zb.Bytes()
	}()
	for name, payload := range map[string][]byte{"not gzip": []byte("plain text pretending"), "a bomb": bomb} {
		h := signed("POST", PathPush, "n", t0, string(payload))
		h["Content-Encoding"] = "gzip"
		code, resp := raw(t, srv, "POST", PathPush, h, string(payload))
		if code != http.StatusBadRequest || !strings.Contains(resp, "gzip") {
			t.Errorf("%s: %d %.120s, want 400 about the gzip body", name, code, resp)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The writer reuses its buffer and encoder: a frame of 100 ops costs a small, fixed number of allocations.
func TestWritingAFrameAllocatesLittle(t *testing.T) {
	ops := make([]WireOp, frameOps)
	for i := range ops {
		ops[i] = WireOp{Node: "node-a", Seq: int64(i + 1), TS: 1, Kind: "task.created", Payload: []byte(`{"id":"t.1","title":"x"}`), Hash: strings.Repeat("a", 64)}
	}
	rec := httptest.NewRecorder()
	fw := newFrameWriter(rec, key, "sig", false)
	fw.write(frame{Ops: ops}) // warm the buffers
	rec.Body.Reset()
	allocs := testing.AllocsPerRun(50, func() {
		rec.Body.Reset()
		fw.write(frame{Ops: ops})
	})
	// Encoding 100 ops needs about 3 allocations per op's RawMessage handling at most; without reuse it was
	// several times this. The bound is a ceiling on regressions, not a target.
	if allocs > allocCeiling {
		t.Fatalf("writing a frame of %d ops allocated %.0f times, want %d or fewer (is the buffer or encoder no longer reused?)", frameOps, allocs, allocCeiling)
	}
	t.Logf("%.0f allocations per frame of %d ops", allocs, frameOps)
}

// An end marker that miscounts is refused even though its mac is right: a peer with the key cannot make the
// receiver believe it got everything when it did not.
func TestAnEndMarkerThatMiscountsIsRefused(t *testing.T) {
	clk := &fakeClock{t0}
	st := newStore(t, "node-local", clk)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case PathSummary:
			b, _ := json.Marshal(SummaryResponse{Node: "liar", Protocol: ProtocolVersion, MinProtocol: MinProtocol, Summary: map[string]int64{}})
			w.Header().Set(HeaderSig, SignResponse(key, r.Header.Get(HeaderSig), 200, b))
			w.Write(b)
		case PathOps:
			w.Header().Set("Content-Type", ContentTypeFrames)
			fw := newFrameWriter(w, key, r.Header.Get(HeaderSig), false)
			fw.write(frame{End: true, N: 7}) // says seven, sent none
			fw.close()
		}
	}))
	defer srv.Close()
	cl := &Client{Peer: srv.URL, Key: key, Now: clk.now}
	if _, err := cl.Once(context.Background(), st); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("err = %v, want ErrBadFrame", err)
	}
}

// A busy answer carries Retry-After and the client passes it on to its back-off.
func TestBusyAnswersCarryRetryAfter(t *testing.T) {
	s := &server{opt: ServerOptions{Key: key}, rep: nil}
	rec := httptest.NewRecorder()
	s.send(rec, "", fail(http.StatusServiceUnavailable, CodeBusy, "slow down"))
	if rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("headers: %v", rec.Header())
	}
	resp := rec.Result()
	re := remoteError(resp, rec.Body.Bytes())
	if re.Code != CodeBusy || re.RetryAfter != time.Second {
		t.Fatalf("remote error = %+v, want busy with a 1s Retry-After", re)
	}
	// nonsense or huge hints are ignored
	for _, v := range []string{"-5", "abc", "99999"} {
		resp.Header.Set("Retry-After", v)
		if got := remoteError(resp, nil).RetryAfter; got != 0 {
			t.Errorf("Retry-After %q gave %v, want it ignored", v, got)
		}
	}
}
