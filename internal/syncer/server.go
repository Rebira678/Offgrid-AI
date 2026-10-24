package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"offgrid/internal/store/sqlstore"
)

// maxBody bounds a request. A batch of DefaultBatch ops with 64 KiB payloads
// would be larger, but real payloads are a few hundred bytes; this keeps a
// stranger from making the server buffer gigabytes.
const maxBody = 16 << 20

// ServerOptions configures Handler.
type ServerOptions struct {
	Key    []byte
	Window time.Duration    // zero means DefaultWindow
	Now    func() time.Time // zero means time.Now
	Log    *log.Logger      // nil means no logging
	// MaxBatch caps how many ops one /sync/ops response holds (zero: DefaultBatch).
	MaxBatch int
}

// Handler serves the sync endpoints for rep.
func Handler(rep Replica, opt ServerOptions) http.Handler {
	if opt.Window == 0 {
		opt.Window = DefaultWindow
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.MaxBatch == 0 {
		opt.MaxBatch = DefaultBatch
	}
	s := &server{rep: rep, opt: opt}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathSummary, s.wrap(s.summary))
	mux.HandleFunc("POST "+PathOps, s.wrap(s.ops))
	mux.HandleFunc("POST "+PathPush, s.wrap(s.push))
	return mux
}

type server struct {
	rep Replica
	opt ServerOptions
}

// reply is what a handler returns; wrap signs and writes it.
type reply struct {
	status int
	body   any
}

func fail(status int, code, msg string) reply {
	return reply{status, ErrorResponse{Code: code, Error: msg}}
}

// wrap reads the body, checks the signature, runs h, and signs the response.
// Nothing reaches h unless the request is authentic and fresh.
func (s *server) wrap(h func(ctx context.Context, node string, body []byte) reply) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || len(body) > maxBody {
			s.send(w, "", fail(http.StatusRequestEntityTooLarge, CodeBadRequest, "the request body is too large or unreadable"))
			return
		}
		node, err := verifyRequest(s.opt.Key, r, body, s.opt.Now(), s.opt.Window)
		if err != nil {
			var ae *authError
			errors.As(err, &ae)
			s.logf("refused %s %s: %s", r.Method, r.URL.Path, ae.Msg)
			// An unauthenticated caller gets no signature: it could not be checked by them either.
			s.send(w, "", fail(http.StatusUnauthorized, ae.Code, ae.Msg))
			return
		}
		rep := h(r.Context(), node, body)
		s.send(w, r.Header.Get(HeaderSig), rep)
	}
}

func (s *server) send(w http.ResponseWriter, requestSig string, rep reply) {
	b, err := json.Marshal(rep.body)
	if err != nil {
		rep = fail(http.StatusInternalServerError, CodeInternal, "cannot encode the response")
		b, _ = json.Marshal(rep.body)
	}
	w.Header().Set("Content-Type", "application/json")
	if requestSig != "" {
		w.Header().Set(HeaderSig, SignResponse(s.opt.Key, requestSig, rep.status, b))
		w.Header().Set(HeaderNode, s.rep.NodeID())
	}
	w.WriteHeader(rep.status)
	w.Write(b)
}

func (s *server) logf(format string, a ...any) {
	if s.opt.Log != nil {
		s.opt.Log.Printf(format, a...)
	}
}

func (s *server) summary(ctx context.Context, peer string, _ []byte) reply {
	sum, err := s.rep.Summary(ctx)
	if err != nil {
		return fail(http.StatusInternalServerError, CodeInternal, err.Error())
	}
	return reply{http.StatusOK, SummaryResponse{Node: s.rep.NodeID(), Protocol: ProtocolVersion, Summary: sum}}
}

func decode(body []byte, into any) *reply {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		r := fail(http.StatusBadRequest, CodeBadRequest, "the request body is not valid: "+err.Error())
		return &r
	}
	return nil
}

func (s *server) ops(ctx context.Context, peer string, body []byte) reply {
	var req OpsRequest
	if bad := decode(body, &req); bad != nil {
		return *bad
	}
	limit := req.Limit
	if limit < 1 || limit > s.opt.MaxBatch {
		limit = s.opt.MaxBatch
	}
	ops, more, err := s.rep.OpsSince(ctx, req.Have, limit)
	if err != nil {
		return fail(http.StatusInternalServerError, CodeInternal, err.Error())
	}
	s.logf("%s pulled %d ops (more=%v)", peer, len(ops), more)
	return reply{http.StatusOK, OpsResponse{Ops: ToWire(ops), More: more}}
}

func (s *server) push(ctx context.Context, peer string, body []byte) reply {
	var req PushRequest
	if bad := decode(body, &req); bad != nil {
		return *bad
	}
	if len(req.Ops) > 4*s.opt.MaxBatch {
		return fail(http.StatusBadRequest, CodeBadRequest, "too many ops in one push")
	}
	res, err := s.rep.ApplyRemote(ctx, FromWire(req.Ops))
	if err != nil {
		status, code := classify(err)
		s.logf("push from %s refused: %v", peer, err)
		return fail(status, code, err.Error())
	}
	s.logf("%s pushed %d new ops (%d already held, rebuilt=%v)", peer, res.Applied, res.Duplicates, res.Rebuilt)
	return reply{http.StatusOK, PushResponse{Applied: res.Applied, Duplicates: res.Duplicates, Rebuilt: res.Rebuilt}}
}

// classify maps an ApplyRemote failure to an HTTP status and a stable code.
// Everything except a clock problem is the sender's data being unacceptable.
func classify(err error) (int, string) {
	switch {
	case errors.Is(err, sqlstore.ErrGap):
		return http.StatusConflict, CodeGap
	case errors.Is(err, sqlstore.ErrFork):
		return http.StatusConflict, CodeFork
	case errors.Is(err, sqlstore.ErrIDCollision):
		return http.StatusConflict, CodeIDCollision
	case errors.Is(err, sqlstore.ErrClockDrift):
		return http.StatusUnprocessableEntity, CodeClockDrift
	case errors.Is(err, sqlstore.ErrBadOp):
		return http.StatusUnprocessableEntity, CodeBadOp
	}
	return http.StatusInternalServerError, CodeInternal
}
