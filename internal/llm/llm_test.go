package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func validRequest() Request {
	return Request{
		Model: "m",
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "hi"},
		},
	}
}

func TestRequestValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Request)
		ok     bool
	}{
		{"valid", func(r *Request) {}, true},
		{"valid with limits", func(r *Request) { r.MaxTokens = 50; r.Temperature = 0.7; r.Seed = 3 }, true},
		{"no model", func(r *Request) { r.Model = "  " }, false},
		{"no messages", func(r *Request) { r.Messages = nil }, false},
		{"negative max tokens", func(r *Request) { r.MaxTokens = -1 }, false},
		{"negative temperature", func(r *Request) { r.Temperature = -0.1 }, false},
		{"unknown role", func(r *Request) { r.Messages[0].Role = "robot" }, false},
		{"empty message", func(r *Request) { r.Messages[1].Content = " \n" }, false},
		{"last message not from user", func(r *Request) {
			r.Messages = append(r.Messages, Message{Role: RoleAssistant, Content: "hello"})
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validRequest()
			tc.mutate(&req)
			err := req.Validate()
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("got %v, want an error matching ErrInvalidRequest", err)
			}
		})
	}
}

func TestUsageTokensPerSecond(t *testing.T) {
	u := Usage{CompletionTokens: 100, Generation: 4 * time.Second}
	if got := u.TokensPerSecond(); got != 25 {
		t.Errorf("got %v, want 25", got)
	}
	for _, bad := range []Usage{{}, {CompletionTokens: 10}, {Generation: time.Second}} {
		if got := bad.TokensPerSecond(); got != 0 {
			t.Errorf("%+v: got %v, want 0 rather than a division by zero", bad, got)
		}
	}
}

func TestFakeEchoesAndRecordsCalls(t *testing.T) {
	f := &Fake{}
	var pieces []string
	resp, err := f.Chat(context.Background(), validRequest(), func(s string) { pieces = append(pieces, s) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "echo: hi" {
		t.Errorf("Text = %q, want %q", resp.Text, "echo: hi")
	}
	if strings.Join(pieces, "") != resp.Text || len(pieces) != 2 {
		t.Errorf("streamed %q in %d pieces, want the same text in 2", pieces, len(pieces))
	}
	if resp.Usage.CompletionTokens != 2 {
		t.Errorf("CompletionTokens = %d, want 2", resp.Usage.CompletionTokens)
	}
	calls := f.Calls()
	if len(calls) != 1 || calls[0].Model != "m" || len(calls[0].Messages) != 2 {
		t.Errorf("recorded calls = %+v", calls)
	}
}

func TestFakeHandlerCanScriptReplyAndError(t *testing.T) {
	boom := errors.New("boom")
	f := &Fake{Handler: func(_ context.Context, req Request) (string, error) {
		if strings.Contains(lastUserMessage(req), "fail") {
			return "", boom
		}
		return "scripted reply", nil
	}}

	resp, err := f.Chat(context.Background(), validRequest(), nil)
	if err != nil || resp.Text != "scripted reply" {
		t.Fatalf("got %q, %v", resp.Text, err)
	}

	req := validRequest()
	req.Messages[1].Content = "please fail"
	if _, err := f.Chat(context.Background(), req, nil); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the handler's error", err)
	}
}

func TestFakeStopsMidStreamWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &Fake{Handler: func(context.Context, Request) (string, error) { return "one two three four", nil }}

	var pieces []string
	_, err := f.Chat(ctx, validRequest(), func(s string) {
		pieces = append(pieces, s)
		cancel() // the user presses Ctrl-C after the first word
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if len(pieces) != 1 {
		t.Errorf("delivered %d pieces after cancellation, want exactly 1", len(pieces))
	}
}

func TestModelNotFoundErrorUnwraps(t *testing.T) {
	cause := errors.New("http 404")
	err := error(&ModelNotFoundError{Model: "x", Err: cause})
	if !errors.Is(err, cause) {
		t.Error("the underlying cause should stay reachable with errors.Is")
	}
	if !strings.Contains(err.Error(), `"x"`) {
		t.Errorf("message %q should name the model", err.Error())
	}
}
