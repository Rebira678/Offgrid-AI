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
