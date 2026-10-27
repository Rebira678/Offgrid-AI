package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIEdgeDirectStructInitTrailingSlashes(t *testing.T) {
	var requestedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.Write([]byte(`{"model":"gpt-4","choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	}))
	defer srv.Close()

	// Struct initialized directly with multiple trailing slashes
	client := &OpenAICompat{
		BaseURL: srv.URL + "////",
		APIKey:  "test-key",
	}

	resp, err := client.Chat(context.Background(), validRequest(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requestedPath != "/v1/chat/completions" {
		t.Fatalf("expected path /v1/chat/completions, got: %q", requestedPath)
	}
	if resp.Text != "pong" {
		t.Fatalf("unexpected response text: %q", resp.Text)
	}
}

func TestOpenAIEdgeInvalidRequestValidation(t *testing.T) {
	client := NewOpenAICompat("http://127.0.0.1:9999", "key")

	badRequests := []struct {
		name string
		req  Request
	}{
		{"empty model", Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}},
		{"no messages", Request{Model: "gpt-4"}},
		{"negative temperature", Request{Model: "gpt-4", Messages: []Message{{Role: RoleUser, Content: "hi"}}, Temperature: -0.5}},
		{"temperature too high", Request{Model: "gpt-4", Messages: []Message{{Role: RoleUser, Content: "hi"}}, Temperature: 2.5}},
		{"invalid message role", Request{Model: "gpt-4", Messages: []Message{{Role: "alien", Content: "hi"}}}},
	}

	for _, tc := range badRequests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Chat(context.Background(), tc.req, nil)
			if err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
		})
	}
}

func TestOpenAIEdgePreCancelledContext(t *testing.T) {
	serverCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewOpenAICompat(srv.URL, "key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := client.Chat(ctx, validRequest(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if serverCalled {
		t.Fatal("HTTP request was sent despite pre-cancelled context")
	}
}

func TestOpenAIEdgeHTMLResponseBodyOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<!DOCTYPE html><html><body><h1>Proxy Gateway Login</h1></body></html>`))
	}))
	defer srv.Close()

	client := NewOpenAICompat(srv.URL, "key")
	_, err := client.Chat(context.Background(), validRequest(), nil)
	if err == nil {
		t.Fatal("expected error on HTML response with 200 OK, got nil")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("expected 'no choices' error, got: %v", err)
	}
}

func TestOpenAIEdgeLargePayloadAndTokens(t *testing.T) {
	largeContent := strings.Repeat("Deliberate reasoning step. ", 2000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"model": "cloud-reasoner",
			"choices": []map[string]any{
				{
					"message": map[string]string{
						"role":    "assistant",
						"content": largeContent,
					},
				},
			},
			"usage": map[string]int{
				"prompt_tokens":     50,
				"completion_tokens": 12000,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewOpenAICompat(srv.URL, "key")
	var streamed string
	resp, err := client.Chat(context.Background(), validRequest(), func(token string) {
		streamed += token
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != largeContent {
		t.Fatalf("response text does not match large content (len: %d vs %d)", len(resp.Text), len(largeContent))
	}
	if streamed != largeContent {
		t.Fatalf("streamed tokens do not match large content")
	}
	if resp.Usage.CompletionTokens != 12000 {
		t.Fatalf("unexpected completion tokens: %d", resp.Usage.CompletionTokens)
	}
}
