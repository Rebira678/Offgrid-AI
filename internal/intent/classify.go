package intent

import (
	"context"

	"offgrid/internal/llm"
	"offgrid/internal/structured"
)

// Classifier understands worker messages using any llm.Client.
type Classifier struct {
	Client llm.Client
	Model  string

	// UseSchema asks the backend to constrain the reply to the Command schema.
	// It is on in normal use. It is a switch only so the evaluation can measure
	// how much it helps (turn it off and the model is guided by the prompt alone).
	UseSchema bool

	// MaxAttempts bounds how many times the model may try, the first try
	// included. Zero means structured.DefaultMaxAttempts.
	MaxAttempts int
}

// Classify returns the validated command for text. The result always carries
// every attempt (what the model said, why it was rejected), even on failure,
// because the evaluation and the debugging output both need them.
//
// Settings chosen on purpose:
//   - Temperature 0: a decision should be as repeatable as possible.
//   - A fixed non-zero seed: the same message gives the same answer next time.
//   - MaxTokens 200: a command is about 60 tokens, so this is generous but
//     stops a model that will not stop.
func (c Classifier) Classify(ctx context.Context, text string) (structured.Result[Command], error) {
	req := llm.Request{
		Model:       c.Model,
		Messages:    Messages(text),
		MaxTokens:   200,
		Temperature: 0,
		Seed:        1,
	}
	if c.UseSchema {
		req.ResponseSchema = Schema()
	}
	return structured.Complete(ctx, c.Client, req, Validate, structured.Options{MaxAttempts: c.MaxAttempts})
}
