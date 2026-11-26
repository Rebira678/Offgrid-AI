package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ErrRejected means a cloud backend refused our credentials (HTTP 401/403) or
// the request's quota. Retrying the same call will not help; the key or the
// plan must change.
var ErrRejected = errors.New("llm: the backend rejected the credentials or quota")

// OpenAICompat is a Client for any server that speaks the common
// "/v1/chat/completions" protocol (OpenAI, many hosted and self-hosted
// gateways). It is the OPTIONAL cloud half of Tier 2: Offgrid never needs it,
// and uses it only when the person configured a URL and a key, and only while
// the network is up.
//
// It does not stream: a Tier 2 job is a background job and nobody is watching
// tokens appear. To honour the Client contract, onToken (if given) receives the
// whole text as one piece.
type OpenAICompat struct {
	BaseURL string // e.g. https://api.openai.com (no trailing /v1/chat/completions)
	APIKey  string
	HTTP    *http.Client // default: 2 minute timeout

	// NoSchema stops the client sending response_format even when the request has a schema. Some
	// servers reject the field; the client also learns this by itself (see schemaRefused).
	NoSchema bool

	schemaRefused atomic.Bool // a server answered HTTP 400 to a request with a schema, and 200 without it
}

// errSchemaRefused is internal: the server said 400 to a request that carried a response_format.
var errSchemaRefused = errors.New("llm: the backend refused response_format")

var _ Client = (*OpenAICompat)(nil)

// NewOpenAICompat returns a cloud client. The key is only ever sent to baseURL,
// in the Authorization header, and is never logged or put in an error.
func NewOpenAICompat(baseURL, apiKey string) *OpenAICompat {
	return &OpenAICompat{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey}
}

type oaiRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature float64      `json:"temperature"`
	Seed        int          `json:"seed,omitempty"`
	Stream      bool         `json:"stream"`

	// ResponseFormat constrains the reply to a JSON Schema where the server supports it (llama.cpp's
	// server, vLLM, OpenAI). It guarantees shape, never truth; callers still validate.
	ResponseFormat *oaiFormat `json:"response_format,omitempty"`
}

type oaiFormat struct {
	Type       string `json:"type"`
	JSONSchema struct {
		Name   string          `json:"name"`
		Strict bool            `json:"strict"`
		Schema json.RawMessage `json:"schema"`
	} `json:"json_schema"`
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message oaiMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Chat implements Client. A request with a ResponseSchema is sent with response_format; if the server
// answers HTTP 400 to that, the same request is sent once more without it and the client remembers, so
// a server that does not know the field costs one extra round trip, once, not one per message.
func (o *OpenAICompat) Chat(ctx context.Context, req Request, onToken func(string)) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	useSchema := len(req.ResponseSchema) > 0 && !o.NoSchema && !o.schemaRefused.Load()
	resp, err := o.chat(ctx, req, onToken, useSchema)
	if useSchema && errors.Is(err, errSchemaRefused) {
		o.schemaRefused.Store(true)
		return o.chat(ctx, req, onToken, false)
	}
	return resp, err
}

func (o *OpenAICompat) chat(ctx context.Context, req Request, onToken func(string), withSchema bool) (Response, error) {
	body := oaiRequest{Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature, Seed: req.Seed}
	if withSchema {
		f := &oaiFormat{Type: "json_schema"}
		f.JSONSchema.Name, f.JSONSchema.Strict, f.JSONSchema.Schema = "reply", true, req.ResponseSchema
		body.ResponseFormat = f
	}
	for _, m := range req.Messages {
		body.Messages = append(body.Messages, oaiMessage{Role: string(m.Role), Content: m.Content})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	baseURL := strings.TrimRight(o.BaseURL, "/")
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	start := time.Now()
	res, err := client.Do(hreq)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Response{}, cerr
		}
		return Response{}, fmt.Errorf("%w: %v", ErrUnavailable, scrub(err, o.APIKey))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Response{}, cerr
		}
		return Response{}, fmt.Errorf("%w: reading the reply: %v", ErrUnavailable, err)
	}
	var out oaiResponse
	_ = json.Unmarshal(data, &out)
	detail := ""
	if out.Error != nil {
		detail = ": " + out.Error.Message
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests && out.Error != nil && out.Error.Code == "insufficient_quota":
		return Response{}, fmt.Errorf("%w (HTTP %d)%s", ErrRejected, res.StatusCode, detail)
	case res.StatusCode == http.StatusNotFound || out.Error != nil && out.Error.Code == "model_not_found":
		return Response{}, &ModelNotFoundError{Model: req.Model, Err: fmt.Errorf("HTTP %d%s", res.StatusCode, detail)}
	case res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests:
		return Response{}, fmt.Errorf("%w: HTTP %d%s", ErrUnavailable, res.StatusCode, detail)
	case res.StatusCode == http.StatusBadRequest && withSchema:
		return Response{}, errSchemaRefused
	case res.StatusCode != http.StatusOK:
		return Response{}, fmt.Errorf("llm: the backend answered HTTP %d%s", res.StatusCode, detail)
	}
	if len(out.Choices) == 0 {
		return Response{}, errors.New("llm: the backend's reply had no choices")
	}
	text := out.Choices[0].Message.Content
	if onToken != nil && text != "" {
		onToken(text)
	}
	total := time.Since(start)
	model := out.Model
	if model == "" {
		model = req.Model
	}
	return Response{Text: text, Model: model, Usage: Usage{
		PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens,
		TimeToFirstToken: total, Total: total, Generation: total,
	}}, nil
}

// scrub removes the key from an error message, in case a transport echoed a header.
func scrub(err error, key string) error {
	if key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "[key]"))
}
