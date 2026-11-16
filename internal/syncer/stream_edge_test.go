package syncer

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

func TestFrameReaderCorruptedJSON(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	seed := "seed-signature"

	// Construct frame with valid MAC but invalid JSON
	corruptedJSON := []byte(`{"ops":[{"id":"bad"`)
	mac := frameMAC(key, seed, corruptedJSON)

	streamData := fmt.Sprintf("%s %s\n", mac, string(corruptedJSON))
	fr := newFrameReader(strings.NewReader(streamData), key, seed)

	_, err := fr.next()
	if !errors.Is(err, ErrBadFrame) {
		t.Fatalf("expected ErrBadFrame on corrupted JSON, got %v", err)
	}
}

func TestFrameReaderDisallowUnknownFields(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	seed := "seed-signature"

	unknownFieldJSON := []byte(`{"ops":[],"unexpected_attacker_field":"injected"}`)
	mac := frameMAC(key, seed, unknownFieldJSON)

	streamData := fmt.Sprintf("%s %s\n", mac, string(unknownFieldJSON))
	fr := newFrameReader(strings.NewReader(streamData), key, seed)

	_, err := fr.next()
	if !errors.Is(err, ErrBadFrame) {
		t.Fatalf("expected ErrBadFrame on unknown field in frame, got %v", err)
	}
}

func TestFrameReaderOversizedLineExceedsMax(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	seed := "seed-signature"

	// Create reader that feeds > 16 MiB without a newline
	hugePayload := strings.Repeat("A", (16<<20)+1024)
	fr := newFrameReader(strings.NewReader(hugePayload), key, seed)

	_, err := fr.next()
	if !errors.Is(err, ErrBadFrame) {
		t.Fatalf("expected ErrBadFrame for line > 16 MiB, got %v", err)
	}
	if !strings.Contains(err.Error(), "larger than 16777216 bytes") {
		t.Errorf("expected size limit in error message, got %v", err)
	}
}

func TestFrameReaderMissingSpaceSeparator(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	seed := "seed-signature"

	streamData := "invalid-frame-line-without-separator\n"
	fr := newFrameReader(strings.NewReader(streamData), key, seed)

	_, err := fr.next()
	if !errors.Is(err, ErrBadFrame) || !strings.Contains(err.Error(), "no mac") {
		t.Fatalf("expected ErrBadFrame (no mac), got %v", err)
	}
}

func TestFrameReaderMutualTLSNoMACMode(t *testing.T) {
	// key == nil simulates mutual TLS mode where transport provides channel integrity
	rec := httptest.NewRecorder()
	fw := newFrameWriter(rec, nil, "", false)

	f1 := frame{Ops: []WireOp{{Node: "n1", Seq: 1, Kind: "task.created", Payload: []byte(`{}`)}}}
	if err := fw.write(f1); err != nil {
		t.Fatalf("write f1: %v", err)
	}
	fEnd := frame{End: true, More: false, N: 1}
	if err := fw.write(fEnd); err != nil {
		t.Fatalf("write fEnd: %v", err)
	}
	_ = fw.close()

	body := rec.Body.String()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "- ") || !strings.HasPrefix(lines[1], "- ") {
		t.Fatalf("expected lines starting with '- ', got:\n%s", body)
	}

	fr := newFrameReader(strings.NewReader(body), nil, "")
	r1, err := fr.next()
	if err != nil || len(r1.Ops) != 1 {
		t.Fatalf("reading r1: %+v, err=%v", r1, err)
	}
	rEnd, err := fr.next()
	if err != nil || !rEnd.End || rEnd.N != 1 {
		t.Fatalf("reading rEnd: %+v, err=%v", rEnd, err)
	}
}

func TestServerInflateOversizedGzipBomb(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := httptest.NewServer(Handler(st, ServerOptions{Key: key}))
	defer srv.Close()

	// Create gzip bomb: 20 MiB of zeros highly compressed
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	zeros := make([]byte, 1<<20) // 1 MiB chunk
	for i := 0; i < 20; i++ {
		zw.Write(zeros)
	}
	zw.Close()

	bombBytes := zb.Bytes()
	now := time.Now()
	sig := SignRequest(key, http.MethodPost, PathPush, "client", now.Unix(), bombBytes)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+PathPush, bytes.NewReader(bombBytes))
	req.Header.Set(HeaderNode, "client")
	req.Header.Set(HeaderTime, fmt.Sprint(now.Unix()))
	req.Header.Set(HeaderProto, fmt.Sprint(ProtocolVersion))
	req.Header.Set(HeaderSig, sig)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on gzip bomb, got %d", resp.StatusCode)
	}
}

func TestServerInflateInvalidGzipBytes(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := httptest.NewServer(Handler(st, ServerOptions{Key: key}))
	defer srv.Close()

	junk := []byte("definitely-not-valid-gzip-stream-content")
	now := time.Now()
	sig := SignRequest(key, http.MethodPost, PathPush, "client", now.Unix(), junk)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+PathPush, bytes.NewReader(junk))
	req.Header.Set(HeaderNode, "client")
	req.Header.Set(HeaderTime, fmt.Sprint(now.Unix()))
	req.Header.Set(HeaderProto, fmt.Sprint(ProtocolVersion))
	req.Header.Set(HeaderSig, sig)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on invalid gzip, got %d", resp.StatusCode)
	}
}

func TestServerInflateUnsupportedEncoding(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := httptest.NewServer(Handler(st, ServerOptions{Key: key}))
	defer srv.Close()

	payload := []byte(`{"ops":[]}`)
	now := time.Now()
	sig := SignRequest(key, http.MethodPost, PathPush, "client", now.Unix(), payload)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+PathPush, bytes.NewReader(payload))
	req.Header.Set(HeaderNode, "client")
	req.Header.Set(HeaderTime, fmt.Sprint(now.Unix()))
	req.Header.Set(HeaderProto, fmt.Sprint(ProtocolVersion))
	req.Header.Set(HeaderSig, sig)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd") // unsupported

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on unsupported Content-Encoding, got %d", resp.StatusCode)
	}
}

func TestFrameReaderRejectsTrailingInjectedData(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	seed := "seed-sig"

	// 1. HMAC mode: Valid JSON followed by injected trailing bytes
	jsonWithExtra := []byte(`{"ops":[]} {"injected":"data"}`)
	mac := frameMAC(key, seed, jsonWithExtra)
	streamData := fmt.Sprintf("%s %s\n", mac, string(jsonWithExtra))

	fr := newFrameReader(strings.NewReader(streamData), key, seed)
	_, err := fr.next()
	if !errors.Is(err, ErrBadFrame) || !strings.Contains(err.Error(), "extra data after frame JSON") {
		t.Fatalf("expected ErrBadFrame with extra data error, got: %v", err)
	}

	// 2. Mutual TLS mode (key == nil): Leading '-' followed by JSON and trailing garbage
	mtlsStreamData := "- {\"end\":true,\"more\":false,\"n\":0} trailing_junk\n"
	frMTLS := newFrameReader(strings.NewReader(mtlsStreamData), nil, "")
	_, errMTLS := frMTLS.next()
	if !errors.Is(errMTLS, ErrBadFrame) || !strings.Contains(errMTLS.Error(), "extra data after frame JSON") {
		t.Fatalf("expected ErrBadFrame on MTLS mode with trailing data, got: %v", errMTLS)
	}
}

func TestPullBatchRejectsEndMarkerWithOps(t *testing.T) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Server streams an end frame that illegitimately carries ops
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ContentTypeFrames)
		w.WriteHeader(http.StatusOK)
		// Mutual TLS style '-' line: carries both End:true AND ops
		w.Write([]byte("- {\"ops\":[{\"node\":\"srv\",\"seq\":1,\"kind\":\"task.created\",\"payload\":\"{}\"}],\"end\":true,\"n\":1}\n"))
	}))
	defer srv.Close()

	client := &Client{Peer: srv.URL, Node: "cli", Mutual: true, HTTP: srv.Client()}
	var rep Report
	_, _, err = client.pullBatch(ctx, st, sqlstore.Summary{}, &rep)
	if !errors.Is(err, ErrBadFrame) || !strings.Contains(err.Error(), "end marker cannot carry ops") {
		t.Fatalf("expected ErrBadFrame with 'end marker cannot carry ops', got: %v", err)
	}
}

func TestPullBatchEnforcesUpperLimitOnStreamedOps(t *testing.T) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Rogue server streams 15 ops when client batch limit is 2 (upper limit 4 * 2 = 8 ops)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ContentTypeFrames)
		w.WriteHeader(http.StatusOK)
		for i := 1; i <= 5; i++ {
			// Each frame sends 3 ops -> 15 ops total
			op1 := fmt.Sprintf(`{"node":"srv","seq":%d,"ts":%d,"kind":"task.created","payload":{"id":"t-%d","title":"t","site":"","priority":"low"}}`, (i-1)*3+1, 1000+((i-1)*3+1), (i-1)*3+1)
			op2 := fmt.Sprintf(`{"node":"srv","seq":%d,"ts":%d,"kind":"task.created","payload":{"id":"t-%d","title":"t","site":"","priority":"low"}}`, (i-1)*3+2, 1000+((i-1)*3+2), (i-1)*3+2)
			op3 := fmt.Sprintf(`{"node":"srv","seq":%d,"ts":%d,"kind":"task.created","payload":{"id":"t-%d","title":"t","site":"","priority":"low"}}`, (i-1)*3+3, 1000+((i-1)*3+3), (i-1)*3+3)
			frameJS := fmt.Sprintf("- {\"ops\":[%s,%s,%s]}\n", op1, op2, op3)
			w.Write([]byte(frameJS))
		}
		w.Write([]byte("- {\"end\":true,\"n\":15}\n"))
	}))
	defer srv.Close()

	client := &Client{Peer: srv.URL, Node: "cli", Mutual: true, HTTP: srv.Client(), Batch: 2}

	var rep Report
	_, _, err = client.pullBatch(ctx, st, sqlstore.Summary{}, &rep)
	if !errors.Is(err, ErrBadFrame) || !strings.Contains(err.Error(), "peer streamed more ops than batch limit") {
		t.Fatalf("expected ErrBadFrame with 'peer streamed more ops than batch limit', got: %v", err)
	}
}

func TestServerInflateEmptyGzipPayload(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{NodeID: "srv"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := httptest.NewServer(Handler(st, ServerOptions{Key: key}))
	defer srv.Close()

	now := time.Now()
	empty := []byte{}
	sig := SignRequest(key, http.MethodPost, PathPush, "client", now.Unix(), empty)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+PathPush, bytes.NewReader(empty))
	req.Header.Set(HeaderNode, "client")
	req.Header.Set(HeaderTime, fmt.Sprint(now.Unix()))
	req.Header.Set(HeaderProto, fmt.Sprint(ProtocolVersion))
	req.Header.Set(HeaderSig, sig)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on empty gzip body, got %d", resp.StatusCode)
	}
}
