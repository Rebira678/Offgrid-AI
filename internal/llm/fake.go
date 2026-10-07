package llm

import (
	"context"
	"strings"
	"sync"
)

// Fake is a Client for tests. It needs no model, no network and no GPU, so
// tests that use it run in microseconds and never flake.
//
// It lives in a normal (non _test) file and is exported on purpose: the agent
// tests on Day 5, the router tests on Day 10 and the queue tests on Day 12 all
// need a model they can script.
//
// A Fake follows the full Client contract, including validation, streaming and
// cancellation, so code that passes with a Fake behaves the same way with a
// real backend.
type Fake struct {
	// Handler produces the reply text, or an error. If nil, the Fake echoes the
	// last user message back with the prefix "echo: ".
	Handler func(ctx context.Context, req Request) (string, error)

	mu    sync.Mutex
	calls []Request
}

var _ Client = (*Fake)(nil)

// Chat implements Client. The reply is streamed one word at a time.
func (f *Fake) Chat(ctx context.Context, req Request, onToken func(string)) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Response{}, err
	}

	text := "echo: " + lastUserMessage(req)
	if f.Handler != nil {
		var err error
		if text, err = f.Handler(ctx, req); err != nil {
			return Response{}, err
		}
	}

	tokens := 0
	for _, piece := range strings.SplitAfter(text, " ") {
		if piece == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		if onToken != nil {
			onToken(piece)
		}
		tokens++
	}
	return Response{Text: text, Model: req.Model, Usage: Usage{CompletionTokens: tokens}}, nil
}

// Calls returns a copy of every request the Fake has received, oldest first.
func (f *Fake) Calls() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.calls...)
}

func lastUserMessage(req Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == RoleUser {
			return req.Messages[i].Content
		}
	}
	return ""
}
