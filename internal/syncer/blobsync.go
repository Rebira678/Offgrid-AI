package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"offgrid/internal/blobstore"
	"offgrid/internal/store/sqlstore"
)

// Blob sync: attachments' bytes move between replicas after the ops that name them.
//
// The exchange is "have and want": one POST /sync/blobs/check carries what the caller WANTS (hashes it
// is missing) and what it OFFERS (hashes it holds). The answer says which wanted blobs the peer HAS and
// which offered blobs the peer NEEDS (with the offset a half-received copy should continue from), so a
// blob a peer already holds is never sent, and a blob nobody asked for is never requested. Then:
//
//	GET /sync/blob/{hash}              the bytes; honours "Range: bytes=N-" so a pull resumes
//	PUT /sync/blob/{hash}?size=N       the bytes from X-Offgrid-Offset on; resumes from the peer's partial
//
// The JSON check is authenticated and signed like the op endpoints. The blob requests carry no body to sign
// (HMAC mode signs the method, path, node and time; mutual TLS is checked as for ops): their integrity is
// the SHA-256 in the path, checked on receipt, and a peer may only upload a blob an op names, with the size
// that op declares. Nothing about a blob transfer is trusted beyond its hash.

const (
	// PathBlobCheck is the have/want exchange.
	PathBlobCheck = "/sync/blobs/check"
	// PathBlob is the prefix of one blob's endpoint: PathBlob + hash.
	PathBlob = "/sync/blob/"
	// HeaderOffset says which byte of the blob a PUT body starts at.
	HeaderOffset = "X-Offgrid-Offset"

	// MaxCheck bounds the hashes in one direction of one check.
	MaxCheck = 500
	// BlobServerTimeout is how long the server lets one blob request run (the server's usual read and write
	// timeouts suit JSON batches, not 25 MiB over a weak link; a cut transfer resumes anyway).
	BlobServerTimeout = 15 * time.Minute

	CodeNoBlobs   = "no_blobs"
	CodeNoSpace   = "no_space"
	CodeOffset    = "offset"
	CodeNotWanted = "not_wanted"
	CodeNotFound  = "not_found"
	CodeBadBlob   = "bad_blob"
	CodeBlobBusy  = "blob_busy"
)

// BlobReplica is a Replica that also keeps attachments' bytes. *sqlstore.Store is one.
type BlobReplica interface {
	Replica
	Blobs() (*blobstore.Store, error)
	MissingBlobs(ctx context.Context, limit int) ([]sqlstore.BlobRef, error)
	HeldBlobs(ctx context.Context, after string, limit int) ([]sqlstore.BlobRef, error)
	BlobWanted(ctx context.Context, hash string, size int64) (bool, error)
}

var _ BlobReplica = (*sqlstore.Store)(nil)

// BlobCheckRequest is the caller's wants and offers.
type BlobCheckRequest struct {
	Want  []string           `json:"want,omitempty"`
	Offer []sqlstore.BlobRef `json:"offer,omitempty"`
}

// BlobNeed is an offered blob the peer wants, and how many bytes of it it already holds.
type BlobNeed struct {
	Hash   string `json:"hash"`
	Offset int64  `json:"offset"`
}

// BlobRefusal is an offered blob the peer would take but cannot, and why.
type BlobRefusal struct {
	Hash   string `json:"hash"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// BlobCheckResponse answers a check.
type BlobCheckResponse struct {
	Have    []string      `json:"have,omitempty"`
	Need    []BlobNeed    `json:"need,omitempty"`
	Refused []BlobRefusal `json:"refused,omitempty"`
}

type blobHandler func(w http.ResponseWriter, r *http.Request, peer string)

func (s *server) blobRoutes(mux *http.ServeMux, blob func(blobHandler) http.HandlerFunc, jsonWrap func(func(context.Context, string, []byte) reply) http.HandlerFunc) {
	mux.HandleFunc("POST "+PathBlobCheck, jsonWrap(s.blobCheck))
	mux.HandleFunc("GET "+PathBlob+"{hash}", blob(s.blobGet))
	mux.HandleFunc("PUT "+PathBlob+"{hash}", blob(s.blobPut))
}

// wrapBlob authenticates a blob request in shared-key mode. There is no body to cover.
func (s *server) wrapBlob(h blobHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		node, err := verifyRequest(s.opt.Key, r, nil, s.opt.Now(), s.opt.Window)
		if err != nil {
			var ae *authError
			errors.As(err, &ae)
			s.logf("refused %s %s: %s", r.Method, r.URL.Path, ae.Msg)
			blobError(w, http.StatusUnauthorized, ae.Code, ae.Msg)
			return
		}
		if bad := checkProto(r); bad != nil {
			blobError(w, bad.status, bad.body.(ErrorResponse).Code, bad.body.(ErrorResponse).Error)
			return
		}
		h(w, r, node)
	}
}

// secureBlob authenticates a blob request on a mutual-TLS connection, with every check ops get.
func (s *server) secureBlob(h blobHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dev, bad := s.tlsPeer(r)
		if bad != nil {
			e := bad.body.(ErrorResponse)
			blobError(w, bad.status, e.Code, e.Error)
			return
		}
		h(w, r, dev)
	}
}

// blobError writes a JSON error. Blob replies are not signed: the bytes are checked by hash.
func blobError(w http.ResponseWriter, status int, code, msg string, extra ...any) {
	body := map[string]any{"code": code, "error": msg}
	for i := 0; i+1 < len(extra); i += 2 {
		body[extra[i].(string)] = extra[i+1]
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *server) blobReplica() (BlobReplica, *blobstore.Store, *reply) {
	br, ok := s.rep.(BlobReplica)
	if !ok {
		r := fail(http.StatusNotImplemented, CodeNoBlobs, "this replica does not keep attachments")
		return nil, nil, &r
	}
	bs, err := br.Blobs()
	if err != nil {
		r := fail(http.StatusNotImplemented, CodeNoBlobs, "this replica has nowhere to keep attachments: "+err.Error())
		return nil, nil, &r
	}
	return br, bs, nil
}

func (s *server) blobCheck(ctx context.Context, peer string, body []byte) reply {
	br, bs, bad := s.blobReplica()
	if bad != nil {
		return *bad
	}
	var req BlobCheckRequest
	if rep := decode(body, &req); rep != nil {
		return *rep
	}
	if len(req.Want) > MaxCheck || len(req.Offer) > MaxCheck {
		return fail(http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("a check carries at most %d hashes each way", MaxCheck))
	}
	var resp BlobCheckResponse
	for _, h := range req.Want {
		if !blobstore.ValidHash(h) {
			return fail(http.StatusBadRequest, CodeBadRequest, "a hash is 64 lowercase hex characters")
		}
		if bs.Has(h) {
			resp.Have = append(resp.Have, h)
		}
	}
	for _, o := range req.Offer {
		if !blobstore.ValidHash(o.Hash) || o.Size < 1 {
			return fail(http.StatusBadRequest, CodeBadRequest, "an offer needs a valid hash and a size")
		}
		if bs.Has(o.Hash) {
			continue // already here: do not send it
		}
		wanted, err := br.BlobWanted(ctx, o.Hash, o.Size)
		if err != nil {
			return fail(http.StatusInternalServerError, CodeInternal, "cannot read the attachment list")
		}
		if !wanted {
			continue // no op here names it with that size: not wanted
		}
		off := bs.PartialSize(o.Hash)
		if off >= o.Size {
			off = 0
		}
		if err := bs.CheckSpace(o.Size - off); err != nil {
			resp.Refused = append(resp.Refused, BlobRefusal{Hash: o.Hash, Code: CodeNoSpace, Reason: err.Error()})
			continue
		}
		resp.Need = append(resp.Need, BlobNeed{Hash: o.Hash, Offset: off})
	}
	return reply{status: http.StatusOK, body: resp}
}

func (s *server) blobGet(w http.ResponseWriter, r *http.Request, peer string) {
	_, bs, bad := s.blobReplica()
	if bad != nil {
		blobError(w, bad.status, CodeNoBlobs, bad.body.(ErrorResponse).Error)
		return
	}
	h := r.PathValue("hash")
	if !blobstore.ValidHash(h) {
		blobError(w, http.StatusBadRequest, CodeBadRequest, "a hash is 64 lowercase hex characters")
		return
	}
	f, err := bs.Open(h)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			blobError(w, http.StatusNotFound, CodeNotFound, "this replica does not hold that blob")
			return
		}
		blobError(w, http.StatusInternalServerError, CodeInternal, "cannot read the blob")
		return
	}
	defer f.Close()
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(BlobServerTimeout))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", time.Time{}, f) // Range, Content-Length and streaming, without reading the file into memory
}

func (s *server) blobPut(w http.ResponseWriter, r *http.Request, peer string) {
	br, bs, bad := s.blobReplica()
	if bad != nil {
		blobError(w, bad.status, CodeNoBlobs, bad.body.(ErrorResponse).Error)
		return
	}
	h := r.PathValue("hash")
	size, serr := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
	offset, oerr := strconv.ParseInt(r.Header.Get(HeaderOffset), 10, 64)
	if !blobstore.ValidHash(h) || serr != nil || size < 1 || oerr != nil || offset < 0 {
		blobError(w, http.StatusBadRequest, CodeBadRequest, "a blob upload needs a valid hash, ?size= and "+HeaderOffset)
		return
	}
	wanted, err := br.BlobWanted(r.Context(), h, size)
	if err != nil {
		blobError(w, http.StatusInternalServerError, CodeInternal, "cannot read the attachment list")
		return
	}
	if !wanted {
		blobError(w, http.StatusForbidden, CodeNotWanted, "no attachment here names that blob with that size")
		return
	}
	if bs.Has(h) {
		blobError(w, http.StatusOK, "already_stored", "this replica already holds that blob")
		return
	}
	tr, err := bs.Begin(h, size)
	switch {
	case errors.Is(err, blobstore.ErrNoSpace):
		blobError(w, http.StatusInsufficientStorage, CodeNoSpace, err.Error())
		return
	case errors.Is(err, blobstore.ErrBusy):
		blobError(w, http.StatusConflict, CodeBlobBusy, err.Error())
		return
	case errors.Is(err, blobstore.ErrTooLarge):
		blobError(w, http.StatusRequestEntityTooLarge, CodeBadRequest, err.Error())
		return
	case err != nil:
		blobError(w, http.StatusInternalServerError, CodeInternal, "cannot start receiving: "+err.Error())
		return
	}
	defer tr.Close()
	if offset != tr.Offset() {
		blobError(w, http.StatusConflict, CodeOffset, fmt.Sprintf("this replica holds %d bytes of that blob, not %d", tr.Offset(), offset), "offset", tr.Offset())
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(BlobServerTimeout))
	if err := tr.Copy(r.Context(), r.Body); err != nil {
		switch {
		case errors.Is(err, blobstore.ErrNoSpace):
			blobError(w, http.StatusInsufficientStorage, CodeNoSpace, err.Error())
		case errors.Is(err, blobstore.ErrSizeMismatch):
			blobError(w, http.StatusBadRequest, CodeBadBlob, err.Error())
		default: // the connection was cut: keep what arrived, the sender resumes
			s.logf("blob %s from %s cut at %d bytes: %v", h[:12], peer, tr.Offset(), err)
			blobError(w, http.StatusBadRequest, CodeBadBlob, "the upload was cut")
		}
		return
	}
	if _, err := tr.Commit(); err != nil {
		switch {
		case errors.Is(err, blobstore.ErrHashMismatch):
			s.logf("blob from %s did not match %s", peer, h[:12])
			blobError(w, http.StatusUnprocessableEntity, CodeBadBlob, "the bytes do not match the hash; the partial copy was discarded")
		case errors.Is(err, blobstore.ErrSizeMismatch):
			blobError(w, http.StatusBadRequest, CodeBadBlob, err.Error()+": send the rest")
		case errors.Is(err, blobstore.ErrNoSpace):
			blobError(w, http.StatusInsufficientStorage, CodeNoSpace, err.Error())
		default:
			blobError(w, http.StatusInternalServerError, CodeInternal, "cannot store the blob: "+err.Error())
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"stored":true}` + "\n"))
}

// ---- client

// BlobOptions tunes SyncBlobs; the zero value is sensible.
type BlobOptions struct {
	Parallel int           // transfers at once (default 2: a phone on weak Wi-Fi gains nothing from more)
	Retries  int           // resumed attempts per blob in one pass after a cut (default 3; negative: none)
	Backoff  time.Duration // pause between attempts (default 500 ms; negative: none)
	Timeout  time.Duration // one blob, all attempts (default 10 minutes)
	NoPush   bool          // only fetch what is missing here
}

func (o BlobOptions) parallel() int {
	if o.Parallel > 0 {
		return o.Parallel
	}
	return 2
}

func (o BlobOptions) retries() int {
	switch {
	case o.Retries < 0:
		return 0
	case o.Retries == 0:
		return 3
	}
	return o.Retries
}

func (o BlobOptions) backoff() time.Duration {
	switch {
	case o.Backoff < 0:
		return 0
	case o.Backoff == 0:
		return 500 * time.Millisecond
	}
	return o.Backoff
}

func (o BlobOptions) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return 10 * time.Minute
}

// BlobReport says what a blob pass did.
type BlobReport struct {
	Pulled      int      // blobs fetched from the peer
	Pushed      int      // blobs the peer took from us
	PulledBytes int64    // bytes received, including the parts of resumed transfers
	PushedBytes int64    // bytes sent
	Resumed     int      // transfers that continued from a partial copy instead of starting over
	Retried     int      // attempts after a cut connection
	Corrupt     int      // blobs whose bytes did not match their hash (discarded, not retried this pass)
	Skipped     int      // the peer lacked it, or another transfer of it was already running
	Refused     []string // why blobs were not moved: a full disk here or there, with the numbers
	Unsupported bool     // the peer does not do blobs
	Requests    int
}

// SyncBlobs moves attachment bytes both ways. It asks for what is missing here and has been seen on the
// peer, offers what is held here and missing there, and transfers each blob (a few at a time) with resume. A
// blob that fails does not stop the others; the first error is returned with the report.
func (c *Client) SyncBlobs(ctx context.Context, local BlobReplica, opt BlobOptions) (BlobReport, error) {
	var rep BlobReport
	bs, err := local.Blobs()
	if err != nil {
		if errors.Is(err, sqlstore.ErrNoBlobStore) {
			return rep, nil
		}
		return rep, err
	}
	wants, err := local.MissingBlobs(ctx, MaxCheck)
	if err != nil {
		return rep, err
	}
	wantSize := map[string]int64{}
	var wantHashes []string
	for _, w := range wants {
		wantSize[w.Hash] = w.Size
		wantHashes = append(wantHashes, w.Hash)
	}

	var (
		tally blobTally
		g     = newGroup(opt.parallel())
		stop  atomic.Bool // set when a disk is full here: asking for more would only fail the same way
		after string
		errs  []error
	)
	for {
		var offers []sqlstore.BlobRef
		if !opt.NoPush {
			if offers, err = local.HeldBlobs(ctx, after, MaxCheck); err != nil {
				errs = append(errs, err)
				break
			}
		}
		if len(wantHashes) == 0 && len(offers) == 0 {
			break
		}
		req := BlobCheckRequest{Want: wantHashes, Offer: offers}
		var resp BlobCheckResponse
		rep.Requests++
		if err := c.call(ctx, http.MethodPost, PathBlobCheck, req, &resp); err != nil {
			var re *RemoteError
			if errors.As(err, &re) && (re.Status == http.StatusNotFound || re.Status == http.StatusNotImplemented || re.Code == CodeNoBlobs) {
				rep.Unsupported = true
			} else {
				errs = append(errs, err)
			}
			break
		}
		for _, r := range resp.Refused {
			tally.refuse("the peer cannot take " + r.Hash[:12] + ": " + r.Reason)
		}
		sizeOf := map[string]int64{}
		for _, o := range offers {
			sizeOf[o.Hash] = o.Size
		}
		for _, h := range resp.Have {
			ref := sqlstore.BlobRef{Hash: h, Size: wantSize[h]}
			if ref.Size == 0 {
				continue // the peer named a blob we did not ask for
			}
			g.go1(func() error {
				if stop.Load() || ctx.Err() != nil {
					return nil
				}
				err := c.pullOne(ctx, bs, ref, opt, &tally)
				if errors.Is(err, blobstore.ErrNoSpace) {
					stop.Store(true)
				}
				return err
			})
		}
		for _, n := range resp.Need {
			ref := sqlstore.BlobRef{Hash: n.Hash, Size: sizeOf[n.Hash]}
			offset := n.Offset
			if ref.Size == 0 {
				continue // the peer named a blob we did not offer
			}
			g.go1(func() error {
				if ctx.Err() != nil {
					return nil
				}
				return c.pushOne(ctx, bs, ref, offset, opt, &tally)
			})
		}
		wantHashes = nil
		if len(offers) < MaxCheck {
			break
		}
		after = offers[len(offers)-1].Hash
	}
	errs = append(errs, g.wait()...)
	rep.Pulled, rep.Pushed = int(tally.pulled.Load()), int(tally.pushed.Load())
	rep.PulledBytes, rep.PushedBytes = tally.pulledBytes.Load(), tally.pushedBytes.Load()
	rep.Resumed, rep.Retried = int(tally.resumed.Load()), int(tally.retried.Load())
	rep.Corrupt, rep.Skipped = int(tally.corrupt.Load()), int(tally.skipped.Load())
	rep.Refused = tally.refusals()
	rep.Requests += int(tally.requests.Load())
	for _, e := range errs {
		if e != nil && !errors.Is(e, blobstore.ErrNoSpace) {
			return rep, e
		}
	}
	return rep, nil
}

type blobTally struct {
	pulled, pushed, resumed, retried, corrupt, skipped, requests atomic.Int64
	pulledBytes, pushedBytes                                     atomic.Int64
	mu                                                           sync.Mutex
	refused                                                      map[string]bool
}

func (t *blobTally) refuse(why string) {
	t.mu.Lock()
	if t.refused == nil {
		t.refused = map[string]bool{}
	}
	t.refused[why] = true
	t.mu.Unlock()
}

func (t *blobTally) refusals() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.refused))
	for r := range t.refused {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// group runs functions with a cap on how many run at once and collects their errors. (It is what
// errgroup.Group.SetLimit does; the standard library is enough here.)
type group struct {
	sem  chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
	errs []error
}

func newGroup(limit int) *group { return &group{sem: make(chan struct{}, limit)} }

// go1 blocks until a slot is free, then runs f in a goroutine.
func (g *group) go1(f func() error) {
	g.sem <- struct{}{}
	g.wg.Add(1)
	go func() {
		defer func() { <-g.sem; g.wg.Done() }()
		if err := f(); err != nil {
			g.mu.Lock()
			g.errs = append(g.errs, err)
			g.mu.Unlock()
		}
	}()
}

func (g *group) wait() []error { g.wg.Wait(); return g.errs }

// doRaw sends one authenticated request whose body is not covered by the signature (see the top of this file).
func (c *Client) doRaw(ctx context.Context, method, path string, query url.Values, hdr map[string]string, body io.Reader, length int64) (*http.Response, error) {
	u := strings.TrimRight(c.Peer, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = length
		if rc, ok := body.(io.ReadCloser); ok {
			req.Body = rc
		} else {
			req.Body = io.NopCloser(body)
		}
	}
	if c.Node == "" {
		c.Node = "anonymous"
	}
	req.Header.Set(HeaderNode, c.Node)
	req.Header.Set(HeaderProto, strconv.Itoa(ProtocolVersion))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if c.Mutual {
		signSecure(req, c.now())
	} else {
		unix := c.now().Unix()
		req.Header.Set(HeaderTime, strconv.FormatInt(unix, 10))
		req.Header.Set(HeaderSig, SignRequest(c.Key, method, path, c.Node, unix, nil))
	}
	hc := *c.http() // a blob can take minutes: the context bounds it, not a 30 s client timeout
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the peer: %w", err)
	}
	return resp, nil
}

// blobRemoteError reads an error reply.
func blobRemoteError(resp *http.Response) (*RemoteError, int64) {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Code   string `json:"code"`
		Error  string `json:"error"`
		Offset int64  `json:"offset"`
	}
	_ = json.Unmarshal(b, &e)
	if e.Error == "" {
		e.Error = strings.TrimSpace(string(b))
	}
	return &RemoteError{Status: resp.StatusCode, Code: e.Code, Msg: e.Error}, e.Offset
}

func (c *Client) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// pullOne fetches one blob, resuming from any partial copy and after any cut.
func (c *Client) pullOne(ctx context.Context, bs *blobstore.Store, ref sqlstore.BlobRef, opt BlobOptions, t *blobTally) error {
	ctx, cancel := context.WithTimeout(ctx, opt.timeout())
	defer cancel()
	var last error
	for attempt := 0; attempt <= opt.retries(); attempt++ {
		if attempt > 0 {
			t.retried.Add(1)
			c.sleep(ctx, opt.backoff())
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		tr, err := bs.Begin(ref.Hash, ref.Size)
		switch {
		case errors.Is(err, blobstore.ErrBusy):
			t.skipped.Add(1)
			return nil
		case errors.Is(err, blobstore.ErrNoSpace):
			t.refuse("this device is full: " + err.Error())
			return err
		case err != nil:
			return err
		}
		off := tr.Offset()
		hdr := map[string]string{}
		if off > 0 {
			hdr["Range"] = "bytes=" + strconv.FormatInt(off, 10) + "-"
		}
		t.requests.Add(1)
		resp, err := c.doRaw(ctx, http.MethodGet, PathBlob+ref.Hash, nil, hdr, nil, 0)
		if err != nil {
			tr.Close()
			last = err
			continue
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			tr.Close()
			t.skipped.Add(1)
			return nil
		case resp.StatusCode == http.StatusOK && off > 0:
			// the peer ignored the Range and is sending everything: start again from zero
			tr.Abort()
			tr.Close()
			tr, err = bs.Begin(ref.Hash, ref.Size)
			if err != nil {
				resp.Body.Close()
				return err
			}
			off = 0
		case resp.StatusCode == http.StatusPartialContent:
			if !strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes "+strconv.FormatInt(off, 10)+"-") {
				resp.Body.Close()
				tr.Abort()
				tr.Close()
				last = errors.New("the peer answered a Range request with a different range")
				continue
			}
		case resp.StatusCode != http.StatusOK:
			re, _ := blobRemoteError(resp)
			resp.Body.Close()
			tr.Close()
			if resp.StatusCode >= 500 {
				last = re
				continue
			}
			return re
		}
		err = tr.Copy(ctx, resp.Body)
		resp.Body.Close()
		if err == nil {
			var ref2 blobstore.Ref
			ref2, err = tr.Commit()
			if err == nil {
				t.pulled.Add(1)
				t.pulledBytes.Add(ref2.Size)
				if off > 0 {
					t.resumed.Add(1)
				}
				return nil
			}
		}
		tr.Close()
		switch {
		case errors.Is(err, blobstore.ErrHashMismatch):
			t.corrupt.Add(1)
			return nil // the partial is gone; the next pass starts from zero, and no retry hammers a bad peer
		case errors.Is(err, blobstore.ErrNoSpace):
			t.refuse("this device is full: " + err.Error())
			return err
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return err
		}
		last = err // a cut, or a body that ended early: the partial stays, the next attempt resumes
	}
	return fmt.Errorf("blob %s: %w", ref.Hash[:12], last)
}

// pushOne sends one blob the peer asked for, from the offset it already holds.
func (c *Client) pushOne(ctx context.Context, bs *blobstore.Store, ref sqlstore.BlobRef, offset int64, opt BlobOptions, t *blobTally) error {
	ctx, cancel := context.WithTimeout(ctx, opt.timeout())
	defer cancel()
	var last error
	for attempt := 0; attempt <= opt.retries(); attempt++ {
		if attempt > 0 {
			t.retried.Add(1)
			c.sleep(ctx, opt.backoff())
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		f, err := bs.Open(ref.Hash)
		if err != nil {
			return err
		}
		if offset >= ref.Size {
			offset = 0
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		q := url.Values{"size": {strconv.FormatInt(ref.Size, 10)}}
		t.requests.Add(1)
		resp, err := c.doRaw(ctx, http.MethodPut, PathBlob+ref.Hash, q, map[string]string{HeaderOffset: strconv.FormatInt(offset, 10), "Content-Type": "application/octet-stream"}, f, ref.Size-offset)
		f.Close()
		if err != nil {
			last = err
			continue
		}
		switch {
		case resp.StatusCode == http.StatusCreated:
			resp.Body.Close()
			t.pushed.Add(1)
			t.pushedBytes.Add(ref.Size - offset)
			if offset > 0 {
				t.resumed.Add(1)
			}
			return nil
		case resp.StatusCode == http.StatusOK: // the peer got it from someone else meanwhile
			resp.Body.Close()
			t.skipped.Add(1)
			return nil
		}
		re, peerOffset := blobRemoteError(resp)
		resp.Body.Close()
		switch {
		case re.Code == CodeOffset:
			offset = peerOffset // the peer holds more or less than we thought: continue from what it holds
			last = re
		case re.Code == CodeNoSpace:
			t.refuse("the peer is full: " + re.Msg)
			return nil
		case re.Code == CodeNotWanted || re.Code == CodeBlobBusy:
			t.skipped.Add(1)
			return nil
		case re.Code == CodeBadBlob && resp.StatusCode == http.StatusUnprocessableEntity:
			// the peer says what it received does not match the hash: our copy is damaged, or the wire is
			t.corrupt.Add(1)
			return nil
		case resp.StatusCode >= 500 || re.Code == CodeBadBlob:
			last = re // cut or incomplete: ask where it stands next time round
			offset = 0
		default:
			return re
		}
	}
	return fmt.Errorf("blob %s: %w", ref.Hash[:12], last)
}
