package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Answer is a finished Tier 2 answer as the view holds it.
type Answer struct {
	Key      string    `json:"key"`
	Question string    `json:"question"`
	Text     string    `json:"text"`
	Model    string    `json:"model"`
	Source   string    `json:"source"`
	At       time.Time `json:"at"`
	Node     string    `json:"node"`
}

// RecordAnswer writes a Tier 2 answer into the log, so it syncs like every
// other fact. It is idempotent: if an answer for key is already held (from an
// earlier attempt that crashed before the job was marked done, or from another
// replica), nothing is written and recorded is false.
func (s *Store) RecordAnswer(ctx context.Context, key, question, text, model, source string) (recorded bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM answers WHERE key = ?`, key).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		if strings.TrimSpace(key) == "" || strings.TrimSpace(text) == "" {
			return &CorruptOpError{Kind: KindAnswerRecorded, Reason: "an answer needs a key and text"}
		}
		if source != "local" && source != "cloud" {
			return &CorruptOpError{Kind: KindAnswerRecorded, Reason: fmt.Sprintf("an answer's source must be local or cloud, not %q", source)}
		}
		if err := s.record(ctx, tx, KindAnswerRecorded, answerPayload{Key: key, Question: question, Text: text, Model: model, Source: source}); err != nil {
			return err
		}
		recorded = true
		return nil
	})
	return recorded, err
}

// Answers lists every recorded answer, oldest first.
func (s *Store) Answers(ctx context.Context) ([]Answer, error) { return listAnswers(ctx, s.db) }

func listAnswers(ctx context.Context, x execer) ([]Answer, error) {
	return listAnswersWhere(ctx, x, "")
}

func listAnswersWhere(ctx context.Context, x execer, key string) ([]Answer, error) {
	rows, err := x.QueryContext(ctx, `SELECT key, question, text, model, source, at, node FROM answers WHERE (? = '' OR key = ?) ORDER BY at, node, seq`, key, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Answer
	for rows.Next() {
		var a Answer
		var at int64
		if err := rows.Scan(&a.Key, &a.Question, &a.Text, &a.Model, &a.Source, &at, &a.Node); err != nil {
			return nil, err
		}
		a.At = fromNanos(at)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Answer returns the answer held for key, if any.
func (s *Store) Answer(ctx context.Context, key string) (Answer, bool, error) {
	all, err := listAnswersWhere(ctx, s.db, key)
	if err != nil || len(all) == 0 {
		return Answer{}, false, err
	}
	return all[0], true, nil
}
