package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"offgrid/internal/llm"
)

// clientFunc lets a test write a one-off llm.Client as a plain function. It
// works because the interface has a single method.
type clientFunc func(ctx context.Context, req llm.Request, onToken func(string)) (llm.Response, error)

func (f clientFunc) Chat(ctx context.Context, req llm.Request, onToken func(string)) (llm.Response, error) {
	return f(ctx, req, onToken)
}

func fakeReply(text string) *llm.Fake {
	return &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) { return text, nil }}
}

// ---- runAsk, tested with a fake model: no HTTP, no Ollama ----------------

func TestRunAskPrintsAnswerAndSendsSystemPrompt(t *testing.T) {
	fake := fakeReply("A CRDT merges edits without conflicts")
	var out, errOut bytes.Buffer

	err := runAsk(context.Background(), fake, askOptions{
		Model: "tiny:1b", System: "be brief", Prompt: "what is a CRDT?", Temperature: 0.2, MaxTokens: 64,
	}, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}

	if out.String() != "A CRDT merges edits without conflicts\n" {
		t.Errorf("stdout = %q, want the answer plus a trailing newline", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr should be empty without -stats, got %q", errOut.String())
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("model called %d times, want 1", len(calls))
	}
	req := calls[0]
	if req.Model != "tiny:1b" || req.MaxTokens != 64 || req.Temperature != 0.2 {
		t.Errorf("request = %+v", req)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != llm.RoleSystem || req.Messages[1].Content != "what is a CRDT?" {
		t.Errorf("messages = %+v, want the system prompt then the user question", req.Messages)
	}
}

func TestRunAskWithoutSystemPromptSendsOnlyTheQuestion(t *testing.T) {
	fake := fakeReply("ok")
	if err := runAsk(context.Background(), fake, askOptions{Model: "m", Prompt: "hi"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if msgs := fake.Calls()[0].Messages; len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Errorf("messages = %+v, want a single user message", msgs)
	}
}

func TestRunAskStatsGoToStderrOnly(t *testing.T) {
	client := clientFunc(func(_ context.Context, req llm.Request, onToken func(string)) (llm.Response, error) {
		onToken("hello\n")
		return llm.Response{Text: "hello\n", Model: req.Model, Usage: llm.Usage{
			CompletionTokens: 100, Generation: 4 * time.Second,
			TimeToFirstToken: 500 * time.Millisecond, Total: 5 * time.Second,
		}}, nil
	})
	var out, errOut bytes.Buffer
	if err := runAsk(context.Background(), client, askOptions{Model: "m", Prompt: "hi", Stats: true}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\n" {
		t.Errorf("stdout = %q, want only the answer (no extra blank line, no stats)", out.String())
	}
	for _, want := range []string{"m |", "100 tokens", "25.0 tok/s", "first token 0.50s", "total 5.00s"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stats %q are missing %q", errOut.String(), want)
		}
	}
}

func TestRunAskFinishesTheLineWhenCancelledMidAnswer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := clientFunc(func(ctx context.Context, _ llm.Request, onToken func(string)) (llm.Response, error) {
		onToken("partial answ")
		cancel() // Ctrl-C
		return llm.Response{}, ctx.Err()
	})
	var out bytes.Buffer
	err := runAsk(ctx, client, askOptions{Model: "m", Prompt: "hi"}, &out, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if out.String() != "partial answ\n" {
		t.Errorf("stdout = %q, want the partial text followed by a newline", out.String())
	}
}

func TestRunAskPropagatesBackendErrorsWithoutOutput(t *testing.T) {
	down := &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) {
		return "", fmt.Errorf("%w: connection refused", llm.ErrUnavailable)
	}}
	var out bytes.Buffer
	err := runAsk(context.Background(), down, askOptions{Model: "m", Prompt: "hi"}, &out, io.Discard)
	if !errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", out.String())
	}
}

func TestReportAskError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantText string
	}{
		{"backend down", fmt.Errorf("%w: refused", llm.ErrUnavailable), 1, "ollama serve"},
		{"model missing", &llm.ModelNotFoundError{Model: "nope:1b"}, 1, "ollama pull nope:1b"},
		{"timeout", context.DeadlineExceeded, 1, "-timeout"},
		{"ctrl-c", context.Canceled, 130, "cancelled"},
		{"bad request", fmt.Errorf("%w: model is required", llm.ErrInvalidRequest), 2, "model is required"},
		{"anything else", errors.New("weird"), 1, "weird"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if code := reportAskError(&buf, tc.err, "m", time.Minute); code != tc.wantCode {
				t.Errorf("exit code %d, want %d", code, tc.wantCode)
			}
			if !strings.Contains(buf.String(), tc.wantText) {
				t.Errorf("message %q should contain %q", buf.String(), tc.wantText)
			}
		})
	}
}

// ---- the whole command, through a fake Ollama server ---------------------

// chatServer imitates /api/chat. Only the model "tiny:1b" exists. Every request
// body it receives is sent on the returned channel.
func chatServer(t *testing.T) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	bodies := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
		if body["model"] != "tiny:1b" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"model not found"}`)
			return
		}
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"Hi"},"done":false}`)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":" there"},"done":false}`)
		fmt.Fprintln(w, `{"done":true,"eval_count":80,"eval_duration":4000000000}`)
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

func TestAskEndToEndWithArguments(t *testing.T) {
	srv, bodies := chatServer(t)
	var out, errOut bytes.Buffer

	code := runIn([]string{"ask", "-host", srv.URL, "-model", "tiny:1b", "-system", "be brief", "-stats",
		"say", "hello"}, strings.NewReader("ignored"), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, errOut.String())
	}
	if out.String() != "Hi there\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "Hi there\n")
	}
	if !strings.Contains(errOut.String(), "20.0 tok/s") {
		t.Errorf("stats missing from stderr: %q", errOut.String())
	}

	body := <-bodies
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want system + user", len(msgs))
	}
	if user, _ := msgs[1].(map[string]any); user["content"] != "say hello" {
		t.Errorf("prompt words should be joined with spaces, got %v", user["content"])
	}
}

func TestAskReadsPromptFromStdin(t *testing.T) {
	srv, bodies := chatServer(t)
	var out, errOut bytes.Buffer

	code := runIn([]string{"ask", "-host", srv.URL, "-model", "tiny:1b"},
		strings.NewReader("what is a pump?\n"), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, errOut.String())
	}
	body := <-bodies
	msgs, _ := body["messages"].([]any)
	if user, _ := msgs[0].(map[string]any); user["content"] != "what is a pump?" {
		t.Errorf("prompt = %v, want the piped text without the trailing newline", user["content"])
	}
}

func TestAskWithoutAnyPromptIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runIn([]string{"ask"}, strings.NewReader("  \n"), &out, &errOut); code != 2 {
		t.Errorf("exit code %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "no prompt") {
		t.Errorf("stderr should explain the problem: %q", errOut.String())
	}
}

func TestAskModelNotInstalled(t *testing.T) {
	srv, _ := chatServer(t)
	var out, errOut bytes.Buffer
	code := runIn([]string{"ask", "-host", srv.URL, "-model", "huge:70b", "hi"}, strings.NewReader(""), &out, &errOut)
	if code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "ollama pull huge:70b") {
		t.Errorf("stderr should say how to install it: %q", errOut.String())
	}
}

func TestAskWhenOllamaIsNotRunning(t *testing.T) {
	srv, _ := chatServer(t)
	url := srv.URL
	srv.Close()

	var out, errOut bytes.Buffer
	code := runIn([]string{"ask", "-host", url, "-model", "tiny:1b", "hi"}, strings.NewReader(""), &out, &errOut)
	if code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "ollama serve") {
		t.Errorf("stderr should explain how to start Ollama: %q", errOut.String())
	}
}

func TestAskTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select { // a model that never answers
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	var out, errOut bytes.Buffer
	code := runIn([]string{"ask", "-host", srv.URL, "-model", "tiny:1b", "-timeout", "100ms", "hi"},
		strings.NewReader(""), &out, &errOut)
	if code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "no complete answer") {
		t.Errorf("stderr should explain the timeout: %q", errOut.String())
	}
	if strings.Contains(errOut.String(), "ollama serve") {
		t.Error("a timeout must not be reported as 'Ollama is not running'")
	}
}

func TestAskNegativeTemperatureIsRejected(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runIn([]string{"ask", "-temp", "-1", "-model", "tiny:1b", "hi"}, strings.NewReader(""), &out, &errOut)
	if code != 2 {
		t.Errorf("exit code %d, want 2 (invalid request)", code)
	}
}
