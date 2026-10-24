// Package syncer lets two Offgrid replicas exchange the ops each is missing
// over HTTP.
//
// A sync round is three questions and an answer, nothing more:
//
//	GET  /sync/summary   "what do you hold?"     -> {node, summary: {node: highest seq}}
//	POST /sync/ops       "send me what I lack"   -> {ops, more}      (pull)
//	POST /sync/push      "take what you lack"    -> {applied, ...}   (push)
//
// Which ops are missing is decided by comparing version summaries (one number
// per node, see sqlstore.Summary), so a round between replicas that are
// already in step moves a few hundred bytes however large the logs are.
//
// Every request and every response carries an HMAC (auth.go): a replica that
// does not know the shared key can neither ask for ops nor give any. HMAC is
// authentication and integrity, not secrecy: the ops cross the wire readable.
// Use a trusted network or put TLS in front for anything else.
package syncer

import (
	"context"
	"encoding/json"
	"time"

	"offgrid/internal/store/sqlstore"
)

// ProtocolVersion is bumped when the wire format changes incompatibly.
const ProtocolVersion = 1

// Paths of the endpoints.
const (
	PathSummary = "/sync/summary"
	PathOps     = "/sync/ops"
	PathPush    = "/sync/push"
)

// DefaultBatch is how many ops travel in one request or response.
const DefaultBatch = 500

// Replica is what the sync code needs from a store. *sqlstore.Store is one;
// tests can supply others.
type Replica interface {
	NodeID() string
	Summary(ctx context.Context) (sqlstore.Summary, error)
	OpsSince(ctx context.Context, have sqlstore.Summary, limit int) ([]sqlstore.Op, bool, error)
	ApplyRemote(ctx context.Context, ops []sqlstore.Op) (sqlstore.ApplyResult, error)
}

var _ Replica = (*sqlstore.Store)(nil)

// WireOp is an op as it travels. The local log position is not sent: it means
// nothing on another replica.
type WireOp struct {
	Node    string          `json:"node"`
	Seq     int64           `json:"seq"`
	TS      int64           `json:"ts"` // nanoseconds since the Unix epoch
	Counter uint32          `json:"counter"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// ToWire converts store ops for sending.
func ToWire(ops []sqlstore.Op) []WireOp {
	out := make([]WireOp, len(ops))
	for i, op := range ops {
		out[i] = WireOp{op.NodeID, op.Seq, op.TS.UnixNano(), op.Counter, op.Kind, op.Payload}
	}
	return out
}

// FromWire converts received ops back.
func FromWire(ops []WireOp) []sqlstore.Op {
	out := make([]sqlstore.Op, len(ops))
	for i, w := range ops {
		out[i] = sqlstore.Op{NodeID: w.Node, Seq: w.Seq, TS: time.Unix(0, w.TS).UTC(), Counter: w.Counter, Kind: w.Kind, Payload: w.Payload}
	}
	return out
}

// SummaryResponse answers GET /sync/summary.
type SummaryResponse struct {
	Node     string           `json:"node"`
	Protocol int              `json:"protocol"`
	Summary  sqlstore.Summary `json:"summary"`
}

// OpsRequest asks for the ops a replica with summary Have lacks.
type OpsRequest struct {
	Have  sqlstore.Summary `json:"have"`
	Limit int              `json:"limit"`
}

// OpsResponse answers POST /sync/ops.
type OpsResponse struct {
	Ops  []WireOp `json:"ops"`
	More bool     `json:"more"`
}

// PushRequest carries ops to the server.
type PushRequest struct {
	Ops []WireOp `json:"ops"`
}

// PushResponse answers POST /sync/push.
type PushResponse struct {
	Applied    int  `json:"applied"`
	Duplicates int  `json:"duplicates"`
	Rebuilt    bool `json:"rebuilt"`
}

// ErrorResponse is the body of every failure. Code is stable and meant for
// programs; Error is meant for people.
type ErrorResponse struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// Error codes.
const (
	CodeUnauthorized = "unauthorized"
	CodeClockSkew    = "clock_skew"
	CodeBadRequest   = "bad_request"
	CodeGap          = "gap"
	CodeFork         = "fork"
	CodeBadOp        = "bad_op"
	CodeIDCollision  = "id_collision"
	CodeClockDrift   = "clock_drift"
	CodeInternal     = "internal"
)
