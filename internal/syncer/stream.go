package syncer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The v2 pull answer is a stream of lines, one frame each:
//
//	<mac> <json>\n
//
// A frame is either a run of ops ({"ops":[...]}) or the end marker ({"end":true,"more":b,"n":total}).
// Over HMAC the mac of a frame is HMAC(key, "offgrid-frame/2\n" + previous mac + "\n" + the json), where the
// first "previous mac" is the request's own signature. So a frame cannot be moved to another response,
// reordered, or dropped from the middle, and a stream that stops before its end marker is recognised as cut.
// The receiver applies each frame as soon as it has checked it: what arrived before a cut is kept, and the
// next round resumes from the replica's own summary, which is the cursor (a replica's ops have no gaps, so
// "the highest seq I hold per node" says exactly where to go on). Over mutual TLS the channel already
// protects the bytes and the mac is "-".
//
// The ops of one answer are still read from the store as one batch (at most MaxBatch), so the server's
// memory is bounded by the batch, not by the stream: frames spare the encoder one huge buffer and the
// receiver the wait for it.

const (
	// ContentTypeFrames marks a frame stream.
	ContentTypeFrames = "application/x-ndjson"
	// frameOps is how many ops go in one frame.
	frameOps = 100
	// maxFrameLine bounds one frame on the receiving side.
	maxFrameLine = 16 << 20
)

type frame struct {
	Ops  []WireOp `json:"ops,omitempty"`
	End  bool     `json:"end,omitempty"`
	More bool     `json:"more,omitempty"`
	N    int      `json:"n,omitempty"`
}

func frameMAC(key []byte, prev string, js []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("offgrid-frame/2\n"))
	m.Write([]byte(prev))
	m.Write([]byte("\n"))
	m.Write(js)
	return hex.EncodeToString(m.Sum(nil))
}

// frameWriter writes frames through one buffer and one encoder that are reused for every frame.
type frameWriter struct {
	bw   *bufio.Writer
	gz   *gzip.Writer
	fl   http.Flusher
	key  []byte // nil: mutual TLS, no mac
	prev string
	buf  bytes.Buffer
	enc  *json.Encoder
}

func newFrameWriter(w http.ResponseWriter, key []byte, seed string, compress bool) *frameWriter {
	fw := &frameWriter{key: key, prev: seed}
	var dst io.Writer = w
	if compress {
		fw.gz = gzip.NewWriter(w)
		dst = fw.gz
	}
	fw.bw = bufio.NewWriterSize(dst, 32<<10)
	fw.fl, _ = w.(http.Flusher)
	fw.enc = json.NewEncoder(&fw.buf)
	return fw
}

func (fw *frameWriter) write(f frame) error {
	fw.buf.Reset()
	if err := fw.enc.Encode(f); err != nil {
		return err
	}
	js := bytes.TrimRight(fw.buf.Bytes(), "\n")
	mac := "-"
	if fw.key != nil {
		mac = frameMAC(fw.key, fw.prev, js)
		fw.prev = mac
	}
	// bufio's errors are sticky: a failed write shows up in the flush below
	_, _ = fw.bw.WriteString(mac)
	_ = fw.bw.WriteByte(' ')
	_, _ = fw.bw.Write(js)
	_ = fw.bw.WriteByte('\n')
	return fw.flush()
}

// flush pushes what is buffered to the peer, so a frame is on the wire when write returns.
func (fw *frameWriter) flush() error {
	if err := fw.bw.Flush(); err != nil {
		return err
	}
	if fw.gz != nil {
		if err := fw.gz.Flush(); err != nil {
			return err
		}
	}
	if fw.fl != nil {
		fw.fl.Flush()
	}
	return nil
}

func (fw *frameWriter) close() error {
	if err := fw.bw.Flush(); err != nil {
		return err
	}
	if fw.gz != nil {
		return fw.gz.Close()
	}
	return nil
}

// opsStream is a pull answer waiting to be written.
type opsStream struct {
	ops      []WireOp
	more     bool
	compress bool
}

func (s *server) sendStream(w http.ResponseWriter, requestSig string, st *opsStream) {
	w.Header().Set("Content-Type", ContentTypeFrames)
	if st.compress {
		w.Header().Set("Content-Encoding", "gzip")
	}
	if requestSig != "" {
		w.Header().Set(HeaderNode, s.rep.NodeID())
	}
	w.WriteHeader(http.StatusOK)
	var key []byte
	if requestSig != "" {
		key = s.opt.Key
	}
	fw := newFrameWriter(w, key, requestSig, st.compress)
	for i := 0; i < len(st.ops); i += frameOps {
		j := min(i+frameOps, len(st.ops))
		if err := fw.write(frame{Ops: st.ops[i:j]}); err != nil {
			return // the peer went away; it resumes from its own summary
		}
	}
	if err := fw.write(frame{End: true, More: st.more, N: len(st.ops)}); err != nil {
		return
	}
	_ = fw.close() // nothing more can be done for a peer that went away
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}

// ErrStreamCut means a pull answer ended before its end marker: the connection was cut, or the peer stopped.
var ErrStreamCut = errors.New("the peer's answer ended before its end marker")

// ErrBadFrame means a frame failed its check: the stream was altered, reordered or is not from the peer.
var ErrBadFrame = errors.New("a frame of the peer's answer failed its check")

type frameReader struct {
	br   *bufio.Reader
	key  []byte
	prev string
	line []byte
}

func newFrameReader(r io.Reader, key []byte, seed string) *frameReader {
	return &frameReader{br: bufio.NewReaderSize(r, 64<<10), key: key, prev: seed}
}

func (fr *frameReader) readLine() ([]byte, error) {
	fr.line = fr.line[:0]
	for {
		part, err := fr.br.ReadSlice('\n')
		fr.line = append(fr.line, part...)
		if len(fr.line) > maxFrameLine {
			return nil, fmt.Errorf("%w: a frame is larger than %d bytes", ErrBadFrame, maxFrameLine)
		}
		switch {
		case err == nil:
			return fr.line[:len(fr.line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(fr.line) == 0:
			return nil, ErrStreamCut
		default:
			return nil, fmt.Errorf("%w: %v", ErrStreamCut, err)
		}
	}
}

// next returns the next verified frame.
func (fr *frameReader) next() (frame, error) {
	var f frame
	line, err := fr.readLine()
	if err != nil {
		return f, err
	}
	mac, js, ok := bytes.Cut(line, []byte(" "))
	if !ok {
		return f, fmt.Errorf("%w: no mac", ErrBadFrame)
	}
	if fr.key != nil {
		want := frameMAC(fr.key, fr.prev, js)
		if !sigEqual(string(mac), want) {
			return f, fmt.Errorf("%w (wrong key, or something altered, reordered or dropped a frame)", ErrBadFrame)
		}
		fr.prev = want
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("%w: %v", ErrBadFrame, err)
	}
	return f, nil
}
