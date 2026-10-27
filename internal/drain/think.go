package drain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"offgrid/internal/jobqueue"
	"offgrid/internal/llm"
)

// AnswerBook is where finished Tier 2 answers are kept so that they sync like
// everything else (in Offgrid it is the task store's op log). It is an
// interface so this package never imports the store.
type AnswerBook interface {
	// Lookup returns the answer already held for key.
	Lookup(ctx context.Context, key string) (text string, found bool, err error)
	// Record keeps an answer and returns the text that is now the answer for
	// key: the one passed in, or, if an answer for key was already held, that
	// one (first answer wins, on every replica).
	Record(ctx context.Context, a Answer) (stored string, err error)
}

// Answer is a finished Tier 2 answer.
type Answer struct {
	Key      string // the job's idempotency key
	Question string
	Text     string
	Model    string
	Source   string // "local" or "cloud"
}

// ThinkConfig configures the slow-thinking (Tier 2) handler.
type ThinkConfig struct {
	// Local is the 7-8B model on this laptop. It needs no network. May be nil
	// if Cloud is set (then every job waits for the network).
	Local      llm.Client
	LocalModel string
	// LocalNeedsNetwork is true when "Local" is really a bigger Ollama on
	// another machine (the Day 12 -deep-host): its jobs then wait for the network.
	LocalNeedsNetwork bool

	// Cloud is an OPTIONAL bigger model reached over the network. It is used
	// only when it is configured AND Online() says the network is up, and the
	// task data in the prompt then leaves this machine: that is why it is
	// never on by default. If it fails for any reason other than cancellation,
	// the local model answers instead.
	Cloud      llm.Client
	CloudModel string
	Online     func() bool

	// Context supplies the data the answer must be based on.
	Context func(ctx context.Context) (string, error)

	// Book receives the answer. nil means the answer lives only in the job's
	// result in the local queue (the Day 12 behaviour).
	Book AnswerBook
}

// Think handles "deep_think" jobs with two brains: a big local model that runs
// when the machine is idle, and an optional cloud model that runs when the
// network is up. The result is written to the AnswerBook as an op, so it
// reaches the person's other devices the same way a task edit does.
//
// It only reads tasks, so running it again after a crash is safe; the
// AnswerBook makes the repeat cheap (the answer is already there: no model is
// asked) and keeps the first answer if two attempts ever disagree.
func Think(cfg ThinkConfig) Spec {
	cloudOnly := cfg.Local == nil
	return Spec{
		Background:   true,
		NeedsNetwork: cloudOnly || cfg.LocalNeedsNetwork,
		Handle: func(ctx context.Context, job jobqueue.Job, _ Progress) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			text, err := decodeText(job.Payload)
			if err != nil {
				return "", err
			}
			if cfg.Book != nil {
				if have, found, err := cfg.Book.Lookup(ctx, job.Key); err != nil {
					return "", fmt.Errorf("looking for an earlier answer: %w", err)
				} else if found {
					return truncate(have), nil
				}
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			data := ""
			if cfg.Context != nil {
				if data, err = cfg.Context(ctx); err != nil {
					return "", fmt.Errorf("reading the task data: %w", err)
				}
			}
			req := llm.Request{
				Messages: []llm.Message{
					{Role: llm.RoleSystem, Content: deepSystem},
					{Role: llm.RoleUser, Content: "Task data:\n" + strings.TrimSpace(data) + "\n\nQuestion: " + text},
				},
				MaxTokens:   400,
				Temperature: 0.2,
			}

			var out, model, source string
			if cfg.Cloud != nil && cfg.Online != nil && cfg.Online() {
				req.Model = cfg.CloudModel
				resp, cerr := cfg.Cloud.Chat(ctx, req, nil)
				switch {
				case cerr == nil && strings.TrimSpace(resp.Text) != "":
					out, model, source = strings.TrimSpace(resp.Text), cfg.CloudModel, "cloud"
				case ctx.Err() != nil:
					return "", ctx.Err()
				case cfg.Local == nil:
					return "", fmt.Errorf("the cloud model failed and there is no local model: %w", orEmpty(cerr))
				}
			}
			if out == "" {
				if cfg.Local == nil {
					return "", errors.New("no local model, and the network is down") // not reached: NeedsNetwork gates it
				}
				req.Model = cfg.LocalModel
				resp, err := cfg.Local.Chat(ctx, req, nil)
				var nf *llm.ModelNotFoundError
				switch {
				case errors.As(err, &nf):
					return "", Permanent(fmt.Errorf("the model %q is not installed on this laptop (ollama pull %s): %w", nf.Model, nf.Model, err))
				case err != nil:
					return "", err
				}
				out, model, source = strings.TrimSpace(resp.Text), cfg.LocalModel, "local"
			}
			if out == "" {
				return "", errors.New("the model returned an empty answer")
			}
			out = truncate(out)
			if cfg.Book != nil {
				stored, err := cfg.Book.Record(ctx, Answer{Key: job.Key, Question: text, Text: out, Model: model, Source: source})
				if err != nil {
					return "", fmt.Errorf("saving the answer: %w", err)
				}
				out = truncate(stored)
			}
			return out, nil
		},
	}
}

func orEmpty(err error) error {
	if err == nil {
		return errors.New("the model returned an empty answer")
	}
	return err
}

func truncate(s string) string {
	if len(s) <= maxResultBytes {
		return s
	}
	s = s[:maxResultBytes]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
