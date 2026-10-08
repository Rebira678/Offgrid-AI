// Package llm is Offgrid's provider-neutral view of a language model.
//
// The rest of Offgrid (the agent, the router, the queue workers) talks to a
// model only through the Client interface in this package. It never imports
// Ollama, never sees an HTTP status code, and never needs to know whether the
// answer came from a 1B model on this laptop, a 7B model, a cloud API, or a
// test double. Swapping the backend is then a one-line change in main.
//
// Offline-first design depends on one thing in particular: a backend that is
// down must be reported in a way callers can recognise (ErrUnavailable), so
// the router can answer "defer this request to the queue" instead of failing.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Role says who wrote a message in a conversation.
type Role string

const (
	RoleSystem    Role = "system"    // instructions that frame the whole conversation
	RoleUser      Role = "user"      // what the person (or the program) is asking
	RoleAssistant Role = "assistant" // what the model said earlier
)

// Message is one turn of a conversation.
type Message struct {
	Role    Role
	Content string
}

// Request is everything a backend needs to produce one reply.
//
// The zero value of an optional field always means "use the sensible
// default", never "literally zero":
//
//   - MaxTokens 0 means no limit (the model stops when it decides to).
//   - Seed 0 means a fresh random seed on every call. Set a non-zero Seed
//     (together with Temperature 0) when you need repeatable output.
//
// Temperature 0 is a real setting: always pick the most likely token. It is
// what agent decisions should use.
type Request struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature float64
	Seed        int

	// ResponseSchema, when non-empty, is a JSON Schema the reply must match.
	// A backend that supports it constrains generation, so the reply is
	// well-formed JSON of that shape. It guarantees SHAPE, never TRUTH: a
	// reply can match the schema and still be wrong, so callers must always
	// validate the content too (see internal/structured). A backend that
	// cannot constrain output ignores it, which is why callers must not rely
	// on it alone.
	ResponseSchema json.RawMessage
}

// Validate reports whether the request is well formed. Implementations call
// it before touching the backend, so a bad request fails fast and the same
// way everywhere.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("%w: at least one message is required", ErrInvalidRequest)
	}
	if r.MaxTokens < 0 {
		return fmt.Errorf("%w: max tokens must not be negative", ErrInvalidRequest)
	}
	if r.Temperature < 0 {
		return fmt.Errorf("%w: temperature must not be negative", ErrInvalidRequest)
	}
	if len(r.ResponseSchema) > 0 && !json.Valid(r.ResponseSchema) {
		return fmt.Errorf("%w: response schema is not valid JSON", ErrInvalidRequest)
	}
	for i, m := range r.Messages {
		switch m.Role {
		case RoleSystem, RoleUser, RoleAssistant:
		default:
			return fmt.Errorf("%w: message %d has unknown role %q", ErrInvalidRequest, i, m.Role)
		}
		if strings.TrimSpace(m.Content) == "" {
			return fmt.Errorf("%w: message %d is empty", ErrInvalidRequest, i)
		}
	}
	if r.Messages[len(r.Messages)-1].Role != RoleUser {
		return fmt.Errorf("%w: the last message must be from the user", ErrInvalidRequest)
	}
	return nil
}

// Usage describes what a call cost, in tokens and time.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TimeToFirstToken time.Duration // call started -> first piece of text
	Total            time.Duration // call started -> call finished
	Generation       time.Duration // time spent producing the completion tokens
}

// TokensPerSecond is the generation speed, or 0 if the backend did not report
// enough to compute it.
func (u Usage) TokensPerSecond() float64 {
	if u.CompletionTokens <= 0 || u.Generation <= 0 {
		return 0
	}
	return float64(u.CompletionTokens) / u.Generation.Seconds()
}

// Response is a finished reply.
type Response struct {
	Text  string
	Model string
	Usage Usage
}

// Errors every Client reports in the same way. Callers test for them with
// errors.Is and errors.As; the text of an error is for humans only.
var (
	// ErrInvalidRequest means the request was malformed. No backend was contacted.
	ErrInvalidRequest = errors.New("llm: invalid request")

	// ErrUnavailable means the backend could not be reached at all (not
	// running, no network path). Retrying later may work. This is the signal
	// the router will use to switch to offline behaviour.
	ErrUnavailable = errors.New("llm: backend unavailable")
)

// ModelNotFoundError means the backend is reachable but does not have the
// requested model. Retrying will not help; the model must be installed.
type ModelNotFoundError struct {
	Model string
	Err   error // the backend's own error, kept for debugging
}

func (e *ModelNotFoundError) Error() string {
	return fmt.Sprintf("llm: model %q is not available", e.Model)
}

func (e *ModelNotFoundError) Unwrap() error { return e.Err }

// Client is a language-model backend.
//
// The contract, which every implementation must follow (llm's contract tests
// check each point against every implementation):
//
//  1. Chat validates the request first. A bad request returns an error
//     matching ErrInvalidRequest without contacting the backend.
//  2. Chat honours ctx. When ctx is cancelled or its deadline passes, Chat
//     returns promptly with an error for which errors.Is(err, ctx.Err()) is
//     true.
//  3. If onToken is not nil it is called with each piece of text as it is
//     produced, one call at a time and on the goroutine that called Chat, so
//     callers need no locking. On success the pieces joined together equal
//     Response.Text. onToken may be nil.
//  4. An unreachable backend is reported as ErrUnavailable and a missing
//     model as *ModelNotFoundError.
//  5. A Client is safe for concurrent use by multiple goroutines.
//
// The interface has a single method on purpose: the smaller it is, the
// cheaper it is to implement, wrap and fake.
type Client interface {
	Chat(ctx context.Context, req Request, onToken func(string)) (Response, error)
}
