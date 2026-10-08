// Package structured gets a typed, validated value out of a language model.
//
// A model's reply is untrusted text. Complete turns it into a Go value in
// three layers of defence, each catching what the one before it cannot:
//
//  1. The request can ask the backend to constrain generation to a JSON
//     schema (llm.Request.ResponseSchema). That makes the SHAPE reliable.
//  2. Complete parses the reply strictly into T and runs your validate
//     function. That catches replies that have the right shape but the wrong
//     meaning (a task with an empty title).
//  3. If either step rejects the reply, Complete shows the model what it said
//     and why it was refused, and tries again, up to a fixed limit. Then it
//     gives up with an error the caller can act on, instead of looping forever.
//
// Complete retries only replies that were REJECTED. It never retries a failed
// call (backend down, timeout, model missing): that is infrastructure, and
// the queue (Day 9) owns it. Mixing the two would hammer a dead server and hide
// the outage the router needs to see.
package structured

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"offgrid/internal/llm"
)

// DefaultMaxAttempts is used when Options.MaxAttempts is zero: one try and
// two retries. Past that, a model that has failed three times in a row with
// feedback is rarely going to succeed on the fourth.
const DefaultMaxAttempts = 3

// ErrNoValidReply means the model was asked several times and never produced
// an acceptable reply.
var ErrNoValidReply = errors.New("structured: no valid reply")

// Options tune Complete.
type Options struct {
	// MaxAttempts is the most times the model may be asked, the first try
	// included. 0 means DefaultMaxAttempts; 1 means no retries.
	MaxAttempts int
}

// Attempt records one round trip with the model.
type Attempt struct {
	Raw      string    // exactly what the model said
	Parsed   bool      // true if Raw was well-formed JSON of the expected shape
	Repaired bool      // true if surrounding text (a code fence, a sentence) had to be cut off first
	Err      error     // why the reply was rejected; nil for the accepted attempt
	Usage    llm.Usage // what the call cost
}

// Result is the outcome of Complete. Attempts is filled in even when Complete
// fails, because the interesting question is usually "what did it say?".
type Result[T any] struct {
	Value    T
	Attempts []Attempt
}

// Succeeded reports whether the last attempt was accepted.
func (r Result[T]) Succeeded() bool {
	n := len(r.Attempts)
	return n > 0 && r.Attempts[n-1].Err == nil
}

// Complete asks c for a reply to req, decodes it into a T, and checks it with
// validate (which may be nil, and may tidy the value in place).
//
// req.Messages is never modified; retries work on a copy.
func Complete[T any](ctx context.Context, c llm.Client, req llm.Request, validate func(*T) error, opt Options) (Result[T], error) {
	limit := opt.MaxAttempts
	if limit == 0 {
		limit = DefaultMaxAttempts
	}
	if limit < 0 {
		return Result[T]{}, fmt.Errorf("%w: MaxAttempts must not be negative", llm.ErrInvalidRequest)
	}

	var res Result[T]
	var lastErr error
	msgs := append([]llm.Message(nil), req.Messages...)

	for n := 1; n <= limit; n++ {
		r := req
		r.Messages = msgs
		resp, err := c.Chat(ctx, r, nil)
		if err != nil {
			return res, err // not a bad reply: see the package comment
		}

		at := Attempt{Raw: resp.Text, Usage: resp.Usage}
		value, repaired, perr := parse[T](resp.Text)
		at.Repaired = repaired
		if perr == nil {
			at.Parsed = true
			if validate != nil {
				perr = validate(&value)
			}
		}
		at.Err = perr
		res.Attempts = append(res.Attempts, at)

		if perr == nil {
			res.Value = value
			return res, nil
		}

		lastErr = perr
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Content: nonEmpty(resp.Text)},
			llm.Message{Role: llm.RoleUser, Content: feedback(perr)},
		)
	}
	return res, fmt.Errorf("%w after %d attempts: %w", ErrNoValidReply, limit, lastErr)
}

// feedback is what the model is told when a reply is refused. Retrying with
// the identical input would be pointless at temperature 0 (the same input
// gives the same output), so the retry must change the input, and the most
// useful change is the exact reason.
func feedback(err error) string {
	return "Your reply was rejected: " + err.Error() + "\nReply again with only the corrected JSON object."
}

// nonEmpty keeps the conversation valid when the model said nothing: every
// message must have content.
func nonEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty reply)"
	}
	return s
}

// parse decodes raw into a T strictly: unknown keys, wrong types and trailing
// junk are all errors. The second result reports whether text around the JSON
// object had to be removed first.
func parse[T any](raw string) (T, bool, error) {
	var v T
	body, repaired := extractJSON(raw)

	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, repaired, fmt.Errorf("the reply is not the expected JSON object: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return v, repaired, errors.New("the reply has extra text after the JSON object")
	}
	return v, repaired, nil
}

// extractJSON cuts a JSON object out of a reply that wraps it in a Markdown
// code fence or a sentence ("Sure! Here you go: {...}"). With a constrained
// backend this never triggers; it is defence in depth for backends that cannot
// constrain output, and the Repaired flag keeps the evaluation honest about
// how often it was needed.
func extractJSON(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return s, false
	}
	cut := s[start : end+1]
	return cut, cut != s
}
