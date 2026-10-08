package ollama

import (
	"context"
	"encoding/json"
)

// ChatMessage is one turn of a conversation. Role is "system", "user" or
// "assistant".
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the body of POST /api/chat.
type ChatRequest struct {
	Model     string        `json:"model"`
	Messages  []ChatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	KeepAlive string        `json:"keep_alive,omitempty"`
	Options   Options       `json:"options"`

	// Format is a JSON Schema. When set, Ollama constrains generation so the
	// reply must match it (the sampler is not allowed to pick a token that
	// would break the schema). Left empty, the field is omitted entirely.
	Format json.RawMessage `json:"format,omitempty"`
}

// Chat streams a reply to a conversation. It behaves exactly like Generate
// (same Result, same errors, same timing numbers); only the endpoint and the
// shape of the request differ.
func (c *Client) Chat(ctx context.Context, req ChatRequest, onToken func(string)) (Result, error) {
	req.Stream = true
	return c.stream(ctx, "/api/chat", req, req.Model, onToken)
}
