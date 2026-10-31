package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"offgrid/internal/task"
)

// ReplicaInfo is what this log holds from one replica (itself included).
type ReplicaInfo struct {
	Node   string
	Ops    int64     // how many of its ops are held (its sequence is gapless, so this is also its highest seq)
	Newest time.Time // the wall part of the newest stamp among them
}

// Replicas lists every replica this log has ops from, by name. It is a read:
// it changes nothing. A replica shows up here the first time one of its ops
// arrives, so this is "who have I heard from", not "who exists".
func (s *Store) Replicas(ctx context.Context) ([]ReplicaInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node_id, MAX(seq), MAX(ts) FROM ops GROUP BY node_id ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReplicaInfo
	for rows.Next() {
		var r ReplicaInfo
		var ts int64
		if err := rows.Scan(&r.Node, &r.Ops, &ts); err != nil {
			return nil, err
		}
		r.Newest = fromNanos(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Describe is one short line saying what an op did, for screens. It never
// fails: an op it cannot read is described by its kind alone. Text that came
// from a person or a model (a title) is cut to 60 characters.
func (o Op) Describe() string {
	var p struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		Task       string `json:"task"`
		Status     string `json:"status"`
		Priority   string `json:"priority"`
		By         string `json:"by"`
		Resolution string `json:"resolution"`
		Question   string `json:"question"`
	}
	_ = json.Unmarshal(o.Payload, &p) // a bad payload leaves p empty
	id := task.ShortID(p.ID)
	switch o.Kind {
	case KindTaskCreated:
		return fmt.Sprintf("created %s %q", id, clipText(p.Title, 60))
	case KindTaskUpdated:
		var ch []string
		if p.Status != "" {
			ch = append(ch, "status "+p.Status)
		}
		if p.Priority != "" {
			ch = append(ch, "priority "+p.Priority)
		}
		return fmt.Sprintf("changed %s: %s", id, strings.Join(ch, ", "))
	case KindNoteAdded:
		return "added a note to " + id
	case KindNoteRemoved:
		return "removed a note from " + id
	case KindTaskRemoved:
		return "deleted " + id
	case KindAnswerRecorded:
		return fmt.Sprintf("recorded an answer to %q", clipText(p.Question, 40))
	case KindSuggestionMade:
		by := p.By
		if by == "" {
			by = "an AI"
		}
		return fmt.Sprintf("%s suggested a change to %s", by, task.ShortID(p.Task))
	case KindSuggestionResolved:
		return "a suggestion was " + p.Resolution
	}
	return o.Kind
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 1 {
		if s == "" || n < 1 {
			return ""
		}
		return "…"
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
