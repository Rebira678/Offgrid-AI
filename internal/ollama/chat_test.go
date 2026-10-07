package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChatStreamsMessageContent(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		bodies <- body
		writeLines(w,
			`{"message":{"role":"assistant","content":"Hel"},"done":false}`,
			`{"message":{"role":"assistant","content":"lo"},"done":false}`,
			finalChunk, // same timing fields as /api/generate
		)
	}))
	defer srv.Close()

	var pieces []string
	res, err := NewClient(srv.URL).Chat(context.Background(), ChatRequest{
		Model: "m",
		Messages: []ChatMessage{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hi"},
		},
		Options: Options{NumPredict: 16},
	}, func(s string) { pieces = append(pieces, s) })
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if res.Text != "Hello" || len(pieces) != 2 {
		t.Errorf("Text = %q from %d pieces, want %q from 2", res.Text, len(pieces), "Hello")
	}
	if got := res.GenTokensPerSec(); got != 25 {
		t.Errorf("GenTokensPerSec = %v, want 25", got)
	}
	if res.TimeToFirstToken <= 0 {
		t.Errorf("TimeToFirstToken = %v, want > 0", res.TimeToFirstToken)
	}

	body := <-bodies
	if body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want 2", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Errorf("first message = %v, want the system prompt", first)
	}
}

func TestChatModelNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"model 'nope' not found"}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).Chat(context.Background(), ChatRequest{Model: "nope"}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("got %v, want an *APIError with status 404", err)
	}
}
