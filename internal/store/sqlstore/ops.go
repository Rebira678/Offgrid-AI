package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"offgrid/internal/hlc"
	"offgrid/internal/task"
)

// execer is what both *sql.DB and *sql.Tx can do. The apply code is written
// against it so the same function runs inside a live write, during a rebuild,
// and against a scratch database during verification.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// The kinds of operation. An op is a FACT about something that happened
// ("task T3 was created"), never an instruction ("create a task"). Facts are
// immutable: you cannot un-happen them, only record a later fact.
const (
	KindTaskCreated = "task.created"
	KindTaskUpdated = "task.updated"
	KindNoteAdded   = "task.note_added"
	KindTaskRemoved = "task.removed"
	KindNoteRemoved = "note.removed"
	// KindAnswerRecorded: a slow-thinking (Tier 2) job finished and this is
	// what it said. It is a fact about the world, not a change to any task.
	KindAnswerRecorded = "answer.recorded"
	// KindSuggestionMade: an AI proposed a change. It is stored beside the task
	// and changes nothing in it.
	KindSuggestionMade = "suggestion.made"
	// KindSuggestionResolved: a person accepted or dismissed a suggestion.
	// Accepting is the only way a suggestion ever changes a task.
	KindSuggestionResolved = "suggestion.resolved"
)

// Op is one immutable entry in the log.
type Op struct {
	Pos     int64           `json:"pos"`         // position in THIS replica's log
	NodeID  string          `json:"node"`        // the replica that created it
	Seq     int64           `json:"seq"`         // that replica's counter, gapless
	TS      time.Time       `json:"ts"`          // the wall part of the op's hybrid logical clock timestamp (UTC)
	Counter uint32          `json:"hlc_counter"` // its logical part; (TS, Counter) is the timestamp
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"` // everything apply needs; see below
}

// Payloads carry every decision made at write time. That is the rule that makes
// replay trustworthy:
//
//	apply(op) is a pure function of the op. It never reads the clock, never
//	generates an ID, never looks at anything but the op and the current rows.
//
// The ID "T7" is therefore written INTO the task.created payload, and the
// timestamp lives in the op. If apply invented them, replaying the log would
// invent different ones and the rebuilt state would not match the original.
type createdPayload struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Site     string        `json:"site"`
	Priority task.Priority `json:"priority"`
}

type updatedPayload struct {
	ID       string         `json:"id"`
	Status   *task.Status   `json:"status,omitempty"`   // only fields that actually changed are recorded
	Priority *task.Priority `json:"priority,omitempty"` //
}

type notePayload struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// removedPayload records that a task was deleted. Seen is everything the
// deleting replica held at that moment (its version summary). A task edit the
// deleter had NOT seen is concurrent with the delete, and add-wins: the task
// stays. An edit it had seen happened before the delete, and the delete wins.
type removedPayload struct {
	ID   string           `json:"id"`
	Seen map[string]int64 `json:"seen"`
}

// noteRemovedPayload records that one note was removed. It names the note by
// the identity of the op that added it, so it removes exactly that note and
// never a note added concurrently.
type noteRemovedPayload struct {
	ID   string `json:"id"`
	Node string `json:"node"`
	Seq  int64  `json:"seq"`
}

// answerPayload is a finished Tier 2 answer. Key names the question (the job's
// idempotency key), so the same question answered twice is one answer.
type answerPayload struct {
	Key      string `json:"key"`
	Question string `json:"question"`
	Text     string `json:"text"`
	Model    string `json:"model"`
	Source   string `json:"source"` // "local" or "cloud": where the words were produced
}

// suggestionPayload is an AI proposal plus what the task looked like when it
// was made (Base*), so a later reader can tell if a person has since changed it.
type suggestionPayload struct {
	Task         string         `json:"task"`
	Status       *task.Status   `json:"status,omitempty"`
	Priority     *task.Priority `json:"priority,omitempty"`
	Note         string         `json:"note,omitempty"`
	BaseStatus   task.Status    `json:"base_status"`
	BasePriority task.Priority  `json:"base_priority"`
	By           string         `json:"by"`
	Key          string         `json:"key,omitempty"`
}

// resolvedPayload names a suggestion by the identity (node, seq) of the op
// that made it, exactly as a note removal names a note.
type resolvedPayload struct {
	Node       string `json:"node"`
	Seq        int64  `json:"seq"`
	Resolution string `json:"resolution"` // "accepted" or "dismissed"
}

// ErrCorruptLog means the log contains something that cannot be applied: an
// unknown kind, a malformed payload, a reference to a task that never existed,
// or a gap in a replica's sequence.
var ErrCorruptLog = errors.New("the op log is corrupt")

// CorruptOpError says which op is bad and why.
type CorruptOpError struct {
	Pos    int64
	Kind   string
	Reason string
}

func (e *CorruptOpError) Error() string {
	if e.Pos == 0 {
		return fmt.Sprintf("the op log is corrupt: %s", e.Reason)
	}
	return fmt.Sprintf("the op log is corrupt: op at position %d (%s): %s", e.Pos, e.Kind, e.Reason)
}

func (e *CorruptOpError) Is(target error) bool { return target == ErrCorruptLog }

// decodeStrict rejects unknown fields: a typo or a newer payload shape must
// fail loudly, never be silently half-applied.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("extra data after the payload")
	}
	return nil
}

// Stamp is the op's hybrid logical clock timestamp.
func (o Op) Stamp() hlc.Timestamp { return hlc.Timestamp{Wall: nanos(o.TS), Counter: o.Counter} }

func nanos(t time.Time) int64 { return t.UTC().UnixNano() }

// fromNanos converts back. Zero means "never": the zero time.Time.
func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// loadOps reads up to limit ops with pos > after, in log order.
func loadOps(ctx context.Context, x execer, after int64, limit int) ([]Op, error) {
	rows, err := x.QueryContext(ctx,
		`SELECT pos, node_id, seq, ts, counter, kind, payload FROM ops WHERE pos > ? ORDER BY pos LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Op
	for rows.Next() {
		var op Op
		var ts int64
		var payload string
		if err := rows.Scan(&op.Pos, &op.NodeID, &op.Seq, &ts, &op.Counter, &op.Kind, &payload); err != nil {
			return nil, err
		}
		op.TS = fromNanos(ts)
		op.Payload = json.RawMessage(payload)
		out = append(out, op)
	}
	return out, rows.Err()
}
