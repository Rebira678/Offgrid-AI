package main

import (
	"context"
	"os"
	"time"

	"offgrid/internal/llm"
	"offgrid/internal/ollama"
)

// providerSpec is the one place that decides which model server a command talks to (Day 50).
//
// -host (or $OLLAMA_HOST) takes a plain address, as before (Ollama), or kind@address:
//
//	-host llamacpp@http://192.168.1.20:8080     a llama.cpp server on another node of the LAN
//	-host openai@https://api.example.com        any OpenAI-compatible endpoint (key from $OFFGRID_API_KEY)
//	-host ollama@http://pi-full.lan:11434       Ollama on the laptop that has the model
//
// $OFFGRID_PROVIDER sets the same thing once for every command, and an explicit -host still wins over it.
func providerSpec(host string) string {
	if p := os.Getenv("OFFGRID_PROVIDER"); p != "" && host == defaultHost() {
		return p
	}
	return host
}

// chatClient builds the client for a -host value. A server on another machine is retried briefly on
// connection failure (a light node borrowing a full node's model must not give up at the first dropped
// packet); a local one is not. A bad spec or an unsafe key setting comes back as a client whose every
// call fails with that error, so the command reports it in its usual way at its first call.
func chatClient(host string) llm.Client {
	p, err := llm.ParseProvider(providerSpec(host), ollama.DefaultBaseURL)
	if err != nil {
		return failedClient{err}
	}
	c, err := p.Open(os.Getenv("OFFGRID_API_KEY"), llm.RetryPolicy{Attempts: 3, Base: 250 * time.Millisecond, Max: 2 * time.Second})
	if err != nil {
		return failedClient{err}
	}
	return governed(c, !p.Local())
}

type failedClient struct{ err error }

func (f failedClient) Chat(context.Context, llm.Request, func(string)) (llm.Response, error) {
	return llm.Response{}, f.err
}
