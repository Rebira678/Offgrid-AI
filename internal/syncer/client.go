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
	"strconv"
	"strings"
	"time"

	"offgrid/internal/store/sqlstore"
)

// Client runs sync rounds against one peer.
type Client struct {
	Peer     string // base URL, e.g. http://127.0.0.1:7878
	Key      []byte
	Node     string       // this replica's node ID, sent in the X-Offgrid-Node header
	DeviceID string       // this replica's device ID, sent in X-Offgrid-Device-ID (Day 37)
	HTTP     *http.Client // nil means a client with a 30 s timeout; tests and netfault wrap this
	Batch    int          // ops per request; zero means DefaultBatch
	Now      func() time.Time
	// Mutual means the connection is mutual TLS (set HTTP from NewTLSClient): no HMAC, the
	// shared key is not used, and each request carries a fresh request ID and the time.
	Mutual bool
}

// Report says what one round did.
type Report struct {
	Peer        string
	Pulled      int // new ops taken from the peer
	Pushed      int // new ops the peer took from us
	PulledDupes int
	PushedDupes int
	Rebuilt     bool // applying pulled ops made this replica rebuild its view
	Requests    int
	Duration    time.Duration
}

// RemoteError is an error reply from the peer.
type RemoteError struct {
	Status     int
	Code       string
	Msg        string
	RetryAfter time.Duration // the peer's hint for a busy answer; zero if it gave none
}

// Is makes errors.Is(err, ErrProtocol) true for an "upgrade required" refusal.
func (e *RemoteError) Is(target error) bool {
	return target == ErrProtocol && e.Code == CodeUpgradeRequired
}

func (e *RemoteError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("the peer replied with %d (%s): %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("the peer replied with %d: %s", e.Status, e.Msg)
}

// ErrBadSignature means the peer's response was not signed with our key: it is
// not who it claims to be, or the body was changed on the way.
var ErrBadSignature = errors.New("the peer's response is not signed with our key (wrong key, or something altered the response)")

// ErrProtocol means the peer speaks a different protocol version.
var ErrProtocol = errors.New("the peer speaks a different sync protocol version")

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) batch() int {
	if c.Batch > 0 {
		return c.Batch
	}
	return DefaultBatch
}

// do sends one signed request and returns the raw response and the request's signature (for checking the
// reply). A body over 1 KiB is gzip-compressed; the signature covers the bytes as sent.
func (c *Client) do(ctx context.Context, method, path string, in any, acceptGzip bool) (*http.Response, string, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return nil, "", err
		}
	}
	compressed := false
	if len(body) > compressMin {
		var zb bytes.Buffer
		zw := gzip.NewWriter(&zb)
		zw.Write(body)
		if err := zw.Close(); err != nil {
			return nil, "", err
		}
		if zb.Len() < len(body) {
			body, compressed = zb.Bytes(), true
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Peer, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	node := c.Node
	if node == "" {
		node = "anonymous"
	}
	unix := c.now().Unix()
	var sig string
	req.Header.Set(HeaderNode, node)
	req.Header.Set(HeaderProto, strconv.Itoa(ProtocolVersion))
	req.Header.Set("Content-Type", "application/json")
	if c.DeviceID != "" {
		req.Header.Set(HeaderDeviceID, c.DeviceID)
	}
	if compressed {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if acceptGzip {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	if c.Mutual {
		signSecure(req, c.now())
	} else {
		sig = SignRequest(c.Key, method, path, node, unix, body)
		req.Header.Set(HeaderTime, strconv.FormatInt(unix, 10))
		req.Header.Set(HeaderSig, sig)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("cannot reach the peer: %w", err)
	}
	return resp, sig, nil
}

// compressMin is the body size above which a request is compressed.
const compressMin = 1024

func readBody(resp *http.Response) ([]byte, error) {
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading the peer's response: %w", err)
	}
	if len(rb) > maxBody {
		return nil, errors.New("the peer's response is too large")
	}
	return rb, nil
}

// remoteError builds the error for a refusal, with the peer's Retry-After hint if it sent one.
func remoteError(resp *http.Response, rb []byte) *RemoteError {
	var e ErrorResponse
	_ = json.Unmarshal(rb, &e) // best effort: the status code is what matters if the body is not JSON
	re := &RemoteError{Status: resp.StatusCode, Code: e.Code, Msg: e.Error}
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 && n <= 300 {
		re.RetryAfter = time.Duration(n) * time.Second
	}
	return re
}

func (c *Client) judge(resp *http.Response, rb []byte, sig string) error {
	if c.Mutual {
		if resp.StatusCode != http.StatusOK {
			return remoteError(resp, rb)
		}
		return nil
	}
	// An unsigned error is how a server refuses a caller it could not authenticate.
	// Show its message, but trust nothing else about it.
	got := resp.Header.Get(HeaderSig)
	if got == "" && resp.StatusCode == http.StatusUnauthorized {
		var e ErrorResponse
		if json.Unmarshal(rb, &e) == nil && e.Error != "" {
			return &RemoteError{Status: resp.StatusCode, Code: e.Code, Msg: e.Error}
		}
	}
	if !sigEqual(got, SignResponse(c.Key, sig, resp.StatusCode, rb)) {
		return ErrBadSignature
	}
	if resp.StatusCode != http.StatusOK {
		return remoteError(resp, rb)
	}
	return nil
}

// call sends one signed request and decodes the verified response into out.
func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	resp, sig, err := c.do(ctx, method, path, in, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, err := readBody(resp)
	if err != nil {
		return err
	}
	if err := c.judge(resp, rb, sig); err != nil {
		return err
	}
	if err := json.Unmarshal(rb, out); err != nil {
		return fmt.Errorf("the peer's response is not valid: %w", err)
	}
	return nil
}

// pullBatch asks for the ops this replica lacks and applies them frame by frame as they arrive and check out.
// It reports how many ops came, how many were new, and whether the peer has more. An error part-way keeps what
// was applied; the next round starts from the replica's own summary.
func (c *Client) pullBatch(ctx context.Context, local Replica, have sqlstore.Summary, rep *Report) (more bool, got int, err error) {
	resp, sig, err := c.do(ctx, http.MethodPost, PathOps, OpsRequest{Have: have, Limit: c.batch()}, true)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, err := readBody(resp)
		if err != nil {
			return false, 0, err
		}
		if err := c.judge(resp, rb, sig); err != nil {
			return false, 0, err
		}
		return false, 0, &RemoteError{Status: resp.StatusCode, Msg: "unexpected status"}
	}
	var src io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return false, 0, fmt.Errorf("%w: %v", ErrStreamCut, err)
		}
		defer zr.Close()
		src = zr
	}
	var key []byte
	if !c.Mutual {
		key = c.Key
	}
	fr := newFrameReader(src, key, sig)
	for {
		f, err := fr.next()
		if err != nil {
			return false, got, err
		}
		if f.End {
			if len(f.Ops) > 0 {
				return false, got, fmt.Errorf("%w: end marker cannot carry ops", ErrBadFrame)
			}
			if f.N != got {
				return false, got, fmt.Errorf("%w: the end marker says %d ops, %d arrived", ErrBadFrame, f.N, got)
			}
			return f.More, got, nil
		}
		if len(f.Ops) == 0 {
			return false, got, fmt.Errorf("%w: an empty frame", ErrBadFrame)
		}
		if got+len(f.Ops) > 4*c.batch() {
			return false, got, fmt.Errorf("%w: peer streamed more ops than batch limit", ErrBadFrame)
		}
		res, err := local.ApplyRemote(ctx, FromWire(f.Ops))
		if err != nil {
			return false, got, fmt.Errorf("applying the peer's ops: %w", err)
		}
		got += len(f.Ops)
		rep.Pulled += res.Applied
		rep.PulledDupes += res.Duplicates
		rep.Rebuilt = rep.Rebuilt || res.Rebuilt
	}
}

// PeerSummary asks the peer what it holds.
func (c *Client) PeerSummary(ctx context.Context) (SummaryResponse, error) {
	var s SummaryResponse
	if err := c.call(ctx, http.MethodGet, PathSummary, nil, &s); err != nil {
		return s, err
	}
	min := s.MinProtocol
	if min == 0 {
		min = s.Protocol // a v1 peer does not say; it takes only its own version
	}
	switch {
	case s.Protocol < MinProtocol:
		return s, fmt.Errorf("%w: the peer runs sync protocol %d and this device needs at least %d; upgrade Offgrid on the peer", ErrProtocol, s.Protocol, MinProtocol)
	case min > ProtocolVersion:
		return s, fmt.Errorf("%w: the peer needs sync protocol %d or newer and this device speaks %d; upgrade Offgrid on this device", ErrProtocol, min, ProtocolVersion)
	}
	if s.Summary == nil {
		s.Summary = sqlstore.Summary{}
	}
	return s, nil
}

// Once runs one full round: pull what the peer has that we lack, then push what
// we have that it lacks. The two halves are separate transactions on separate
// machines; if the round is cut off between batches, everything applied so far
// is kept, and the next round continues from there.
func (c *Client) Once(ctx context.Context, local Replica) (Report, error) {
	start := c.now()
	rep := Report{Peer: c.Peer}
	defer func() { rep.Duration = c.now().Sub(start) }()
	if c.Node == "" {
		c.Node = local.NodeID()
	}

	peer, err := c.PeerSummary(ctx)
	rep.Requests++
	if err != nil {
		return rep, err
	}
	if peer.Node == local.NodeID() {
		return rep, fmt.Errorf("the peer is this replica (both are %q): each replica needs its own node ID", peer.Node)
	}

	// pull
	for {
		have, err := local.Summary(ctx)
		if err != nil {
			return rep, err
		}
		rep.Requests++
		before := rep.Pulled
		more, got, err := c.pullBatch(ctx, local, have, &rep)
		if err != nil {
			return rep, fmt.Errorf("pull: %w", err)
		}
		if got == 0 {
			if more {
				return rep, errors.New("pull: the peer reported more ops but sent an empty batch; stopping")
			}
			break
		}
		if rep.Pulled == before && more {
			return rep, errors.New("pull: the peer keeps sending ops this replica already has; stopping")
		}
		if !more {
			break
		}
	}

	// push. The peer's summary is advanced by what each batch carried, so no
	// second /sync/summary is needed.
	have := peer.Summary
	if have == nil {
		have = make(sqlstore.Summary)
	}
	for {
		ops, more, err := local.OpsSince(ctx, have, c.batch())
		if err != nil {
			return rep, err
		}
		if len(ops) == 0 {
			break
		}
		var resp PushResponse
		rep.Requests++
		if err := c.call(ctx, http.MethodPost, PathPush, PushRequest{Ops: ToWire(ops)}, &resp); err != nil {
			return rep, fmt.Errorf("push: %w", err)
		}
		rep.Pushed += resp.Applied
		rep.PushedDupes += resp.Duplicates
		advanced := false
		for _, op := range ops {
			if op.Seq > have[op.NodeID] {
				have[op.NodeID] = op.Seq
				advanced = true
			}
		}
		if !advanced && more {
			return rep, errors.New("push: cannot advance push cursor; stopping")
		}
		if !more {
			break
		}
	}
	return rep, nil
}
