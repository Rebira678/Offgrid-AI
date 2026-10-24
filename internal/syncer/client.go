package syncer

import (
	"bytes"
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
	Peer  string // base URL, e.g. http://127.0.0.1:7878
	Key   []byte
	Node  string       // this replica's node ID, sent in the X-Offgrid-Node header
	HTTP  *http.Client // nil means a client with a 30 s timeout; tests and netfault wrap this
	Batch int          // ops per request; zero means DefaultBatch
	Now   func() time.Time
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
	Status int
	Code   string
	Msg    string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("peer refused (%d %s): %s", e.Status, e.Code, e.Msg)
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

// call sends one signed request and decodes the verified response into out.
func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Peer, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if c.Node == "" {
		c.Node = "anonymous"
	}
	unix := c.now().Unix()
	sig := SignRequest(c.Key, method, path, c.Node, unix, body)
	req.Header.Set(HeaderNode, c.Node)
	req.Header.Set(HeaderTime, strconv.FormatInt(unix, 10))
	req.Header.Set(HeaderSig, sig)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the peer: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("reading the peer's response: %w", err)
	}
	if len(rb) > maxBody {
		return errors.New("the peer's response is too large")
	}
	// An unsigned error is how a server refuses a caller it could not authenticate.
	// Show its message, but trust nothing else about it.
	got := resp.Header.Get(HeaderSig)
	if got == "" && resp.StatusCode == http.StatusUnauthorized {
		var e ErrorResponse
		if json.Unmarshal(rb, &e) == nil && e.Error != "" {
			return &RemoteError{resp.StatusCode, e.Code, e.Error}
		}
	}
	if !sigEqual(got, SignResponse(c.Key, sig, resp.StatusCode, rb)) {
		return ErrBadSignature
	}
	if resp.StatusCode != http.StatusOK {
		var e ErrorResponse
		json.Unmarshal(rb, &e)
		return &RemoteError{resp.StatusCode, e.Code, e.Error}
	}
	if err := json.Unmarshal(rb, out); err != nil {
		return fmt.Errorf("the peer's response is not valid: %w", err)
	}
	return nil
}

// PeerSummary asks the peer what it holds.
func (c *Client) PeerSummary(ctx context.Context) (SummaryResponse, error) {
	var s SummaryResponse
	if err := c.call(ctx, http.MethodGet, PathSummary, nil, &s); err != nil {
		return s, err
	}
	if s.Protocol != ProtocolVersion {
		return s, fmt.Errorf("%w (peer %d, here %d)", ErrProtocol, s.Protocol, ProtocolVersion)
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
		var resp OpsResponse
		rep.Requests++
		if err := c.call(ctx, http.MethodPost, PathOps, OpsRequest{Have: have, Limit: c.batch()}, &resp); err != nil {
			return rep, fmt.Errorf("pull: %w", err)
		}
		if len(resp.Ops) == 0 {
			if resp.More {
				return rep, errors.New("pull: the peer reported more ops but sent an empty batch; stopping")
			}
			break
		}
		res, err := local.ApplyRemote(ctx, FromWire(resp.Ops))
		if err != nil {
			return rep, fmt.Errorf("pull: applying the peer's ops: %w", err)
		}
		rep.Pulled += res.Applied
		rep.PulledDupes += res.Duplicates
		rep.Rebuilt = rep.Rebuilt || res.Rebuilt
		if res.Applied == 0 && resp.More {
			return rep, errors.New("pull: the peer keeps sending ops this replica already has; stopping")
		}
		if !resp.More {
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
