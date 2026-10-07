package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"

	"offgrid/internal/ollama"
)

// Ollama is a Client backed by a local Ollama server.
type Ollama struct {
	client *ollama.Client

	// KeepAlive is how long Ollama keeps the model in RAM after a call.
	// Keeping it loaded avoids paying the multi-second load cost on every
	// request. Default "10m".
	KeepAlive string
}

// Compile-time proof that *Ollama satisfies Client. If the interface and the
// implementation ever drift apart, this line stops the build with a clear
// message instead of failing later at a distant call site.
var _ Client = (*Ollama)(nil)

// NewOllama returns a Client that talks to the given Ollama client.
func NewOllama(c *ollama.Client) *Ollama {
	return &Ollama{client: c, KeepAlive: "10m"}
}

// Chat implements Client.
func (o *Ollama) Chat(ctx context.Context, req Request, onToken func(string)) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}

	msgs := make([]ollama.ChatMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = ollama.ChatMessage{Role: string(m.Role), Content: m.Content}
	}

	// Translate our "zero means default" rules into what Ollama expects:
	// num_predict -1 means unlimited, and Ollama treats a seed of 0 as a real,
	// fixed seed, so "random" must be an explicit random number.
	numPredict := req.MaxTokens
	if numPredict <= 0 {
		numPredict = -1
	}
	seed := req.Seed
	if seed == 0 {
		seed = 1 + rand.Intn(1<<31-1)
	}

	res, err := o.client.Chat(ctx, ollama.ChatRequest{
		Model:     req.Model,
		Messages:  msgs,
		KeepAlive: o.KeepAlive,
		Options:   ollama.Options{NumPredict: numPredict, Temperature: req.Temperature, Seed: seed},
	}, onToken)
	if err != nil {
		return Response{}, translateError(err, req.Model)
	}

	return Response{
		Text:  res.Text,
		Model: req.Model,
		Usage: Usage{
			PromptTokens:     res.PromptTokens,
			CompletionTokens: res.GenTokens,
			TimeToFirstToken: res.TimeToFirstToken,
			Total:            res.Wall,
			Generation:       res.Eval,
		},
	}, nil
}

// translateError converts Ollama-specific failures into the package's own
// errors, so callers never have to import the ollama package. Context errors
// pass through untouched: a timeout is not the same thing as an outage.
func translateError(err error, model string) error {
	var apiErr *ollama.APIError
	switch {
	case errors.Is(err, ollama.ErrUnreachable):
		// Both errors stay in the chain: callers can match ErrUnavailable,
		// while the original text still says which address failed.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		return &ModelNotFoundError{Model: model, Err: err}
	default:
		return err
	}
}
