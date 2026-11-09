package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

// Day 34: the sync request parser is the first code a stranger's bytes reach
// on a node that listens on a network. Two properties are fuzzed:
//
//   - FuzzSyncRequestBody: a correctly signed request with ANY body, on any
//     method and path, must never panic and never produce a 5xx; every answer
//     is JSON, and a 2xx answer carries a signature the sender can verify.
//   - FuzzSyncAuth: whatever the headers, path and body, a request is answered
//     2xx only if it carries the right signature for exactly those bytes and a
//     fresh time. Nothing else gets through.

type fuzzStubReplica struct{}

func (fuzzStubReplica) NodeID() string { return "stub" }
func (fuzzStubReplica) Summary(context.Context) (sqlstore.Summary, error) {
	return sqlstore.Summary{"stub": 3}, nil
}
func (fuzzStubReplica) OpsSince(context.Context, sqlstore.Summary, int) ([]sqlstore.Op, bool, error) {
	return nil, false, nil
}
func (fuzzStubReplica) ApplyRemote(_ context.Context, ops []sqlstore.Op) (sqlstore.ApplyResult, error) {
	return sqlstore.ApplyResult{Applied: len(ops)}, nil
}

var (
	fuzzNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fuzzKey = []byte("0123456789abcdef-fuzz-key")
)

func fuzzHandler() http.Handler {
	return Handler(fuzzStubReplica{}, ServerOptions{Key: fuzzKey, Now: func() time.Time { return fuzzNow }})
}

// do builds the request by hand: httptest.NewRequest panics on a bad URL, and a
// real server would just see whatever bytes arrived.
func doReq(h http.Handler, method, path string, hdr map[string]string, body []byte) *httptest.ResponseRecorder {
	r := &http.Request{
		Method: method,
		URL:    &url.URL{Path: path},
		Header: http.Header{},
		Body:   io.NopCloser(bytes.NewReader(body)),
		Host:   "node.lan",
	}
	for k, v := range hdr {
		if v != "" {
			r.Header[http.CanonicalHeaderKey(k)] = []string{v}
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func signedHeaders(method, path string, body []byte) map[string]string {
	unix := fuzzNow.Unix()
	return map[string]string{
		HeaderNode: "fuzz-node",
		HeaderTime: strconv.FormatInt(unix, 10),
		HeaderSig:  SignRequest(fuzzKey, method, path, "fuzz-node", unix, body),
	}
}

var fuzzBodies = []string{
	``, `{}`, `null`, `[]`, `"x"`, `0`, `{`, `{"ops":[]}`, `{"ops":null}`, `{"have":{}}`,
	`{"have":{"a":3,"b":0},"limit":5}`, `{"have":{"a":-1},"limit":-5}`, `{"limit":1e999}`, `{"limit":9223372036854775807}`,
	`{"have":{"":1}}`, `{"have":null,"limit":0}`, `{"unknown":1}`, `{"ops":[]} trailing`, `{"ops":[]}{"ops":[]}`,
	`{"ops":[{"node":"a","seq":1,"ts":1,"counter":0,"kind":"task.created","payload":{"id":"T1","title":"t","site":"","priority":"low"}}]}`,
	`{"ops":[{"node":1}]}`, `{"ops":[{"seq":"x"}]}`, `{"ops":[{"payload":}]}`, `{"ops":[{"ts":1e30}]}`,
	`{"ops":[{"node":"a","seq":9223372036854775807,"ts":-9223372036854775808,"counter":4294967296}]}`,
	"\x00\xff\xfe", `{"ops":` + string(bytes.Repeat([]byte("["), 5000)),
}

var fuzzEndpoints = []struct{ method, path string }{
	{http.MethodGet, PathSummary}, {http.MethodPost, PathOps}, {http.MethodPost, PathPush},
	{http.MethodPost, "/sync/nope"}, {http.MethodGet, PathPush}, {http.MethodPut, PathOps}, {http.MethodPost, "/"},
}

func FuzzSyncRequestBody(f *testing.F) {
	for e := range fuzzEndpoints {
		for _, b := range fuzzBodies {
			f.Add(uint8(e), []byte(b))
		}
	}
	h := fuzzHandler()
	f.Fuzz(func(t *testing.T, endpoint uint8, body []byte) {
		e := fuzzEndpoints[int(endpoint)%len(fuzzEndpoints)]
		w := doReq(h, e.method, e.path, signedHeaders(e.method, e.path, body), body)
		if w.Code >= 500 {
			t.Fatalf("%s %s answered %d (%s) for body %q", e.method, e.path, w.Code, w.Body.String(), body)
		}
		if w.Code == http.StatusUnauthorized {
			t.Fatalf("a correctly signed, fresh request was refused as unauthorized: %s", w.Body.String())
		}
		if !json.Valid(w.Body.Bytes()) && w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%d answer is not JSON: %q", w.Code, w.Body.String())
		}
		if w.Code/100 == 2 {
			want := SignResponse(fuzzKey, signedHeaders(e.method, e.path, body)[HeaderSig], w.Code, w.Body.Bytes())
			if got := w.Header().Get(HeaderSig); got != want {
				t.Fatalf("a %d answer is not signed correctly (got %q, want %q)", w.Code, got, want)
			}
		}
		if w.Code == http.StatusBadRequest {
			var er ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &er); err != nil || er.Code != CodeBadRequest || er.Error == "" {
				t.Fatalf("a 400 must carry the bad_request error body, got %q", w.Body.String())
			}
		}
	})
}

func FuzzSyncAuth(f *testing.F) {
	valid := func(e int, body string) (uint8, string, string, string, string, []byte) {
		m, p := fuzzEndpoints[e].method, fuzzEndpoints[e].path
		s := signedHeaders(m, p, []byte(body))
		sel := uint8(1) // index into methods: 0 GET, 1 POST
		if m == http.MethodGet {
			sel = 0
		}
		return sel, p, s[HeaderNode], s[HeaderTime], s[HeaderSig], []byte(body)
	}
	for e := 0; e < 3; e++ {
		for _, body := range []string{`{}`, `{"ops":[]}`} {
			m, p, n, ts, sig, b := valid(e, body)
			f.Add(m, p, n, ts, sig, b, int32(0), false)
		}
	}
	ep, p, n, ts, sig, b := valid(1, `{}`)
	f.Add(ep, p, n, ts, sig, b, int32(301), false)  // just outside the window
	f.Add(ep, p, n, ts, sig, b, int32(-301), false) // and the other way
	f.Add(ep, p, "", ts, sig, b, int32(0), false)   // no node
	f.Add(ep, p, n, "", sig, b, int32(0), false)    // no time
	f.Add(ep, p, n, ts, "", b, int32(0), false)     // no signature
	f.Add(ep, p, n, "abc", sig, b, int32(0), false) // time is not a number
	f.Add(ep, p, n, "9223372036854775807", sig, b, int32(0), false)
	f.Add(ep, p, n, "-9223372036854775808", sig, b, int32(0), false)
	f.Add(ep, p, n, ts, sig[:len(sig)-1], b, int32(0), false)
	f.Add(ep, p, n, ts, sig+"0", b, int32(0), false)
	f.Add(ep, p+"/", n, ts, sig, b, int32(0), false)
	f.Add(ep, "/sync//ops", n, ts, sig, b, int32(0), false)
	f.Add(ep, p, n+"x", ts, sig, b, int32(0), false)
	f.Add(ep, p, n, ts, sig, append(b, ' '), int32(0), false) // a changed body

	for _, k := range []int32{-1, 1, -299, 299, -300, 300, -301, 301, -3600, 3600, 1 << 30, -1 << 30} {
		f.Add(ep, p, n, ts, sig, b, k, true) // valid MAC, shifted time
	}
	f.Add(ep, p, n, "9223372036854775807", sig, b, int32(0), true)
	f.Add(ep, p, n, "-9223372036854775808", sig, b, int32(0), true)
	f.Add(ep, p, "", ts, sig, b, int32(0), true)

	h := fuzzHandler()
	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead, ""}
	f.Fuzz(func(t *testing.T, mSel uint8, path, node, ts, sig string, body []byte, skew int32, resign bool) {
		method := methods[int(mSel)%len(methods)]
		// ts as given, except that skew lets the fuzzer shift a valid-looking time.
		if skew != 0 {
			if u, err := strconv.ParseInt(ts, 10, 64); err == nil && u > -1<<62 && u < 1<<62 {
				ts = strconv.FormatInt(u+int64(skew), 10)
			}
		}
		// resign: give the request a valid MAC for exactly these inputs, so the fuzzer
		// can reach the checks that come AFTER the signature (the time window).
		if resign {
			if u, err := strconv.ParseInt(ts, 10, 64); err == nil {
				sig = SignRequest(fuzzKey, method, path, node, u, body)
			}
		}
		w := doReq(h, method, path, map[string]string{HeaderNode: node, HeaderTime: ts, HeaderSig: sig}, body)
		unix, perr := strconv.ParseInt(ts, 10, 64)
		authentic := perr == nil && node != "" && sig == SignRequest(fuzzKey, method, path, node, unix, body)
		if authentic {
			if d := fuzzNow.Sub(time.Unix(unix, 0)); d < -DefaultWindow || d > DefaultWindow {
				authentic = false
			}
		}
		if authentic && w.Code == http.StatusUnauthorized {
			t.Fatalf("refused a request with a valid signature and a fresh time (ts %s, %s from the server clock)", ts, fuzzNow.Sub(time.Unix(unix, 0)))
		}
		if w.Code/100 != 2 {
			return
		}
		// Accepted: that is only legitimate for exactly the right MAC and a fresh time.
		if perr != nil || node == "" || sig == "" {
			t.Fatalf("accepted a request with node=%q time=%q sig=%q", node, ts, sig)
		}
		if want := SignRequest(fuzzKey, method, path, node, unix, body); sig != want {
			t.Fatalf("accepted a request whose signature is not for these bytes (method %q path %q)", method, path)
		}
		if d := fuzzNow.Sub(time.Unix(unix, 0)); d < -DefaultWindow || d > DefaultWindow {
			t.Fatalf("accepted a request %s away from the server's clock (window %s)", d, DefaultWindow)
		}
	})
}
