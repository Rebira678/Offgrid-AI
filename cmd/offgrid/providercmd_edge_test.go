package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"offgrid/internal/llm"
	"offgrid/internal/ollama"
)

func TestProviderCmdEnvOverrides(t *testing.T) {
	// 1. Default host with OFFGRID_PROVIDER unset -> returns defaultHost()
	t.Setenv("OFFGRID_PROVIDER", "")
	if s := providerSpec(defaultHost()); s != defaultHost() {
		t.Errorf("expected default host %q, got %q", defaultHost(), s)
	}

	// 2. Default host with OFFGRID_PROVIDER set -> returns OFFGRID_PROVIDER
	customProvider := "llamacpp@http://192.168.1.100:8080"
	t.Setenv("OFFGRID_PROVIDER", customProvider)
	if s := providerSpec(defaultHost()); s != customProvider {
		t.Errorf("expected %q, got %q", customProvider, s)
	}

	// 3. Explicit host specified by caller overrides OFFGRID_PROVIDER
	explicitHost := "http://laptop.lan:11434"
	if s := providerSpec(explicitHost); s != explicitHost {
		t.Errorf("explicit host must override OFFGRID_PROVIDER, got %q", s)
	}
}

func TestProviderCmdFailedClientReturnsError(t *testing.T) {
	// Unknown provider syntax triggers failedClient immediately
	c := chatClient("unknown-provider-kind@http://localhost:8080")
	_, err := c.Chat(context.Background(), llm.Request{Model: "test"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("expected error containing 'unknown provider', got: %v", err)
	}
}

func TestProviderCmdInsecureKeyRejection(t *testing.T) {
	// Plain HTTP to public Internet with key set in OFFGRID_API_KEY must fail with ErrInsecureKey
	t.Setenv("OFFGRID_API_KEY", "secret-token")
	c := chatClient("openai@http://api.together.xyz/v1")
	_, err := c.Chat(context.Background(), llm.Request{Model: "test"}, nil)
	if !errors.Is(err, llm.ErrInsecureKey) {
		t.Fatalf("expected ErrInsecureKey, got: %v", err)
	}
}

func TestProviderCmdLocalVsRemote(t *testing.T) {
	t.Setenv("OFFGRID_API_KEY", "")

	// Local provider: should not fail on creation
	cLocal := chatClient("ollama@" + ollama.DefaultBaseURL)
	if cLocal == nil {
		t.Fatal("expected non-nil local client")
	}

	// Remote provider: should succeed on creation
	cRemote := chatClient("llamacpp@http://192.168.1.50:8080")
	if cRemote == nil {
		t.Fatal("expected non-nil remote client")
	}
}
