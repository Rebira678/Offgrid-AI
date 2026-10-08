package structured

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"offgrid/internal/llm"
)

type payload struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

var errNeedsName = errors.New("name must not be empty")

func validatePayload(p *payload) error {
	if p.Name == "" {
		return errNeedsName
	}
	return nil
}

func request() llm.Request {
	return llm.Request{
		Model:    "m",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
	}
}

// scripted returns a Fake that answers with the given replies, one per call.
func scripted(replies ...string) *llm.Fake {
	var mu sync.Mutex
	i := 0
	return &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(replies) {
			return "", errors.New("test script ran out of replies")
		}
		r := replies[i]
		i++
		return r, nil
	}}
}

func TestFirstTrySuccess(t *testing.T) {
	f := scripted(`{"name":"a","n":1}`)
	res, err := Complete(context.Background(), f, request(), validatePayload, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Value != (payload{"a", 1}) || len(res.Attempts) != 1 || !res.Succeeded() {
		t.Errorf("got %+v", res)
	}
	if at := res.Attempts[0]; !at.Parsed || at.Repaired || at.Err != nil {
		t.Errorf("attempt = %+v", at)
	}
}

func TestRetryShowsTheModelItsMistake(t *testing.T) {
	f := scripted(`{"name":"","n":1}`, `{"name":"fixed","n":1}`)
	req := request()
	res, err := Complete(context.Background(), f, req, validatePayload, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Value.Name != "fixed" || len(res.Attempts) != 2 {
		t.Fatalf("got %+v", res)
	}
	if !res.Attempts[0].Parsed || !errors.Is(res.Attempts[0].Err, errNeedsName) {
		t.Errorf("first attempt = %+v, want parsed but rejected for its content", res.Attempts[0])
	}

	// The second request must contain the bad reply and the reason, and must
	// still end with a user message.
	second := f.Calls()[1].Messages
	n := len(second)
	if n != 3 {
		t.Fatalf("second request has %d messages, want 3 (question, bad reply, feedback)", n)
	}
	if second[1].Role != llm.RoleAssistant || second[1].Content != `{"name":"","n":1}` {
		t.Errorf("second[1] = %+v, want the model's own bad reply", second[1])
	}
	if second[2].Role != llm.RoleUser || !strings.Contains(second[2].Content, "name must not be empty") {
		t.Errorf("second[2] = %+v, want feedback naming the problem", second[2])
	}

	if len(req.Messages) != 1 {
		t.Error("the caller's request was modified")
	}
}

func TestGivesUpAfterTheLimit(t *testing.T) {
	f := scripted(`{"name":""}`, `{"name":""}`, `{"name":""}`, `{"name":"too late"}`)
	res, err := Complete(context.Background(), f, request(), validatePayload, Options{})
	if !errors.Is(err, ErrNoValidReply) {
		t.Fatalf("got %v, want ErrNoValidReply", err)
	}
	if !errors.Is(err, errNeedsName) {
		t.Error("the reason for the last rejection should stay in the error chain")
	}
	if len(f.Calls()) != DefaultMaxAttempts || len(res.Attempts) != DefaultMaxAttempts {
		t.Errorf("made %d calls and kept %d attempts, want %d of each", len(f.Calls()), len(res.Attempts), DefaultMaxAttempts)
	}
	if res.Succeeded() {
		t.Error("Succeeded() must be false")
	}
}

func TestMaxAttemptsOneMeansNoRetry(t *testing.T) {
	f := scripted(`{"name":""}`, `{"name":"x"}`)
	_, err := Complete(context.Background(), f, request(), validatePayload, Options{MaxAttempts: 1})
	if !errors.Is(err, ErrNoValidReply) || len(f.Calls()) != 1 {
		t.Errorf("err = %v after %d calls, want ErrNoValidReply after 1", err, len(f.Calls()))
	}
}

func TestBackendFailureIsNotRetried(t *testing.T) {
	f := &llm.Fake{Handler: func(context.Context, llm.Request) (string, error) {
		return "", llm.ErrUnavailable
	}}
	_, err := Complete(context.Background(), f, request(), validatePayload, Options{})
	if !errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	if errors.Is(err, ErrNoValidReply) {
		t.Error("an outage is not a bad reply")
	}
	if len(f.Calls()) != 1 {
		t.Errorf("made %d calls, want 1: an outage must not be retried here", len(f.Calls()))
	}
}

func TestEmptyReplyIsRejectedAndRetried(t *testing.T) {
	f := scripted("", `{"name":"ok"}`)
	res, err := Complete(context.Background(), f, request(), validatePayload, Options{})
	if err != nil {
		t.Fatalf("an empty reply must be retried, not crash the conversation: %v", err)
	}
	if len(res.Attempts) != 2 || res.Attempts[0].Parsed {
		t.Errorf("attempts = %+v", res.Attempts)
	}
	if got := f.Calls()[1].Messages[1].Content; got != "(empty reply)" {
		t.Errorf("empty reply was echoed back as %q", got)
	}
}

func TestSurroundingTextIsCutOffAndFlagged(t *testing.T) {
	cases := map[string]string{
		"code fence":     "```json\n{\"name\":\"a\",\"n\":2}\n```",
		"sentence first": "Sure! Here you go: {\"name\":\"a\",\"n\":2}",
		"sentence after": "{\"name\":\"a\",\"n\":2} Hope that helps!",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := Complete(context.Background(), scripted(raw), request(), validatePayload, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Value != (payload{"a", 2}) {
				t.Errorf("value = %+v", res.Value)
			}
			if !res.Attempts[0].Repaired {
				t.Error("the attempt should be flagged as repaired, so evaluations can count it")
			}
		})
	}
}

func TestStrictParsing(t *testing.T) {
	cases := map[string]string{
		"unknown key":      `{"name":"a","n":1,"extra":true}`,
		"wrong type":       `{"name":"a","n":"one"}`,
		"truncated":        `{"name":"a","n":`,
		"not json":         `I cannot help with that.`,
		"two objects":      `{"name":"a"}{"name":"b"}`,
		"array not object": `[1,2,3]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := Complete(context.Background(), scripted(raw), request(), validatePayload, Options{MaxAttempts: 1})
			if !errors.Is(err, ErrNoValidReply) {
				t.Fatalf("got %v, want ErrNoValidReply", err)
			}
			if res.Attempts[0].Parsed {
				t.Error("a malformed reply must not count as parsed")
			}
		})
	}
}

func TestContextCancellationStopsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := scripted(`{"name":"a"}`)
	_, err := Complete(ctx, f, request(), validatePayload, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestNegativeMaxAttemptsIsInvalid(t *testing.T) {
	_, err := Complete(context.Background(), scripted(), request(), validatePayload, Options{MaxAttempts: -1})
	if !errors.Is(err, llm.ErrInvalidRequest) {
		t.Fatalf("got %v, want ErrInvalidRequest", err)
	}
}

func TestNilValidatorAcceptsAnyWellFormedValue(t *testing.T) {
	res, err := Complete[payload](context.Background(), scripted(`{"name":"","n":0}`), request(), nil, Options{})
	if err != nil || !res.Succeeded() {
		t.Fatalf("got %v, %+v", err, res)
	}
}

func TestValidatorMayTidyTheValue(t *testing.T) {
	tidy := func(p *payload) error { p.Name = strings.ToUpper(p.Name); return nil }
	res, err := Complete(context.Background(), scripted(`{"name":"abc"}`), request(), tidy, Options{})
	if err != nil || res.Value.Name != "ABC" {
		t.Errorf("got %+v, %v", res.Value, err)
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		repaired bool
	}{
		{`{"a":1}`, `{"a":1}`, false},
		{"  {\"a\":1}\n", `{"a":1}`, false},
		{"```json\n{\"a\":1}\n```", `{"a":1}`, true},
		{`ok {"a":{"b":2}} bye`, `{"a":{"b":2}}`, true},
		{`no braces here`, `no braces here`, false},
		{`} backwards {`, `} backwards {`, false},
	}
	for _, tc := range cases {
		got, repaired := extractJSON(tc.in)
		if got != tc.want || repaired != tc.repaired {
			t.Errorf("extractJSON(%q) = %q, %v; want %q, %v", tc.in, got, repaired, tc.want, tc.repaired)
		}
	}
}
