package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"offgrid/internal/ollama"
)

func captureBodies(t *testing.T) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	bodies := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
		fmt.Fprintln(w, `{"message":{"content":"{}"},"done":false}`)
		fmt.Fprintln(w, `{"done":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

func TestOllamaForwardsSchemaAsFormat(t *testing.T) {
	srv, bodies := captureBodies(t)
	c := NewOllama(ollama.NewClient(srv.URL))

	req := validRequest()
	req.ResponseSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`)
	if _, err := c.Chat(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	format, ok := (<-bodies)["format"].(map[string]any)
	if !ok || format["type"] != "object" {
		t.Errorf("format = %v, want the schema object", format)
	}
}

func TestOllamaOmitsFormatWithoutSchema(t *testing.T) {
	srv, bodies := captureBodies(t)
	if _, err := NewOllama(ollama.NewClient(srv.URL)).Chat(context.Background(), validRequest(), nil); err != nil {
		t.Fatal(err)
	}
	if body := <-bodies; body["format"] != nil {
		t.Errorf("format = %v, want it absent when no schema is requested", body["format"])
	}
}

func TestInvalidSchemaIsRejectedBeforeAnyCall(t *testing.T) {
	req := validRequest()
	req.ResponseSchema = json.RawMessage(`{not json`)
	if err := req.Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("got %v, want ErrInvalidRequest", err)
	}
}

func TestFakeRecordsTheSchema(t *testing.T) {
	f := &Fake{}
	req := validRequest()
	req.ResponseSchema = json.RawMessage(`{"type":"object"}`)
	if _, err := f.Chat(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls()[0].ResponseSchema; string(got) != `{"type":"object"}` {
		t.Errorf("recorded schema = %s", got)
	}
}
