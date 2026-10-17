package drain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"offgrid/internal/jobqueue"
	"offgrid/internal/llm"
)

// TestInterpretHandlerRefusesExecutingMarker verifies the at-most-once safety mechanism:
// if a job crashed after writing the "executing" progress marker, re-running it refuses
// to execute and returns a permanent error instructing a human operator to inspect the state.
func TestInterpretHandlerRefusesExecutingMarker(t *testing.T) {
	r := newRig(creates("Duplicate Task Prevention"))
	p := &progressLog{}

	j := job("create a task for inspecting the generator", markExecuting)

	_, err := Interpret(r.agent).Handle(context.Background(), j, p)
	if err == nil {
		t.Fatal("expected error when progress is 'executing', got nil")
	}
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("expected ErrPermanent, got: %v", err)
	}
	if !strings.Contains(err.Error(), "already have changed your tasks before the program stopped") {
		t.Errorf("unexpected error message: %v", err)
	}

	// Verify NO task was created
	tasks := r.tasks(t)
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks created, found %d: %+v", len(tasks), tasks)
	}
}

// TestInterpretHandlerMalformedJSONPayload tests resilience when given corrupted job payload.
func TestInterpretHandlerMalformedJSONPayload(t *testing.T) {
	r := newRig(creates("Should Not Run"))
	p := &progressLog{}

	j := jobqueue.Job{
		ID:       10,
		Kind:     KindInterpret,
		Payload:  json.RawMessage(`{not-json!`),
		Progress: "",
	}

	_, err := Interpret(r.agent).Handle(context.Background(), j, p)
	if err == nil || !errors.Is(err, ErrPermanent) {
		t.Errorf("expected permanent error on malformed json payload, got: %v", err)
	}
}

// TestDeepThinkNeedsNetworkBasedOnConfig verifies that DeepThink reflects NeedsNetwork.
func TestDeepThinkNeedsNetworkBasedOnConfig(t *testing.T) {
	localDT := DeepThink(DeepThinkConfig{
		NeedsNetwork: false,
		Model:        "llama3.2:3b",
	})
	if localDT.NeedsNetwork {
		t.Error("local DeepThink should not need network")
	}

	remoteDT := DeepThink(DeepThinkConfig{
		NeedsNetwork: true,
		Model:        "llama3.2:3b",
	})
	if !remoteDT.NeedsNetwork {
		t.Error("remote DeepThink must need network")
	}
}

// TestDeepThinkHandlerSurfacesChatClientError verifies that when LLM returns a network/server
// error, the handler bubbles it up for retries (not permanent).
func TestDeepThinkHandlerSurfacesChatClientError(t *testing.T) {
	p := &progressLog{}

	fakeLLM := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			return "", errors.New("upstream model overloaded (HTTP 503)")
		},
	}

	dt := DeepThink(DeepThinkConfig{
		Client:       fakeLLM,
		Model:        "llama3.2:3b",
		NeedsNetwork: true,
	})
	j := jobqueue.Job{
		ID:      15,
		Kind:    KindDeepThink,
		Payload: json.RawMessage(`{"text":"summarize recent incidents"}`),
	}

	_, err := dt.Handle(context.Background(), j, p)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "upstream model overloaded") {
		t.Errorf("unexpected error: %v", err)
	}
	if errors.Is(err, ErrPermanent) {
		t.Error("temporary upstream error should NOT be marked permanent")
	}
}

// TestDeepThinkHandlerModelNotFoundIsPermanent verifies that a missing model error
// is marked Permanent so the worker doesn't endlessly burn retries.
func TestDeepThinkHandlerModelNotFoundIsPermanent(t *testing.T) {
	p := &progressLog{}

	fakeLLM := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			return "", &llm.ModelNotFoundError{Model: "nonexistent-model"}
		},
	}

	dt := DeepThink(DeepThinkConfig{
		Client: fakeLLM,
		Model:  "nonexistent-model",
	})
	j := jobqueue.Job{
		ID:      16,
		Kind:    KindDeepThink,
		Payload: json.RawMessage(`{"text":"summarize recent incidents"}`),
	}

	_, err := dt.Handle(context.Background(), j, p)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("expected ErrPermanent for ModelNotFoundError, got: %v", err)
	}
}
