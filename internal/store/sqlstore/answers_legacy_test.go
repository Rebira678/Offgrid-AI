package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"offgrid/internal/task"
)

func legacyHash(ctx context.Context, s *Store) (string, error) {
	tasks, err := s.List(ctx, task.Filter{})
	if err != nil {
		return "", err
	}
	type note struct {
		Text string `json:"text"`
		At   int64  `json:"at"`
	}
	_ = note{}
	canon := make([]canonTask, 0, len(tasks))
	for _, t := range tasks {
		c := canonTask{ID: t.ID, Title: t.Title, Site: t.Site, Priority: string(t.Priority), Status: string(t.Status),
			Created: nanos(t.CreatedAt), Updated: nanos(t.UpdatedAt), Notes: []canonNote{}}
		if !t.DoneAt.IsZero() {
			c.Done = nanos(t.DoneAt)
		}
		for _, n := range t.Notes {
			c.Notes = append(c.Notes, canonNote{Text: n.Text, At: nanos(n.At)})
		}
		canon = append(canon, c)
	}
	raw, _ := json.Marshal(canon)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
